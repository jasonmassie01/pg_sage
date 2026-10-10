package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// agent_request_capability (AGENTDB-SPEC §8.2): an agent asks for a
// time-boxed, column-listed grant. Agent governance decides the request
// (D1-D10); an allowed one waits for an operator's approval in pg_sage
// (every G1 grant is L2), which runs guard_grant. The agent never grants
// itself anything.

// GrantToolBackend serves agent_request_capability (the grants service,
// adapted in cmd: agent governance imports this package's callers' types).
type GrantToolBackend interface {
	RequestCapability(ctx context.Context, principalID string,
		in CapabilityRequest) (CapabilityResult, error)
}

// CapabilityObject is one table or view asked for, with its columns.
type CapabilityObject struct {
	Object  string   `json:"object"`
	Columns []string `json:"columns,omitempty"`
}

// CapabilityRequest is agent_request_capability's validated input.
type CapabilityRequest struct {
	Database        string
	Capability      string
	Objects         []CapabilityObject
	DurationMinutes int
	Reason          string
}

// Capability request verdicts (§8.1).
const (
	VerdictQueueApproval = "queue_approval"
	VerdictObserveOnly   = "observe_only"
	VerdictBlocked       = "blocked"
	VerdictPark          = "park"
)

// CapabilityResult is agent_request_capability's output (§8.2).
type CapabilityResult struct {
	Verdict           string     `json:"verdict"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	RequestID         int64      `json:"request_id,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	ApprovalURL       string     `json:"approval_url,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	Fix               string     `json:"fix,omitempty"`
	RetryAfterSeconds int        `json:"retry_after,omitempty"`
}

// maxCapabilityObjects bounds one request's objects.
const maxCapabilityObjects = 50

const toolRequestCapability = "agent_request_capability"

func grantTools() []Tool {
	return []Tool{{Name: toolRequestCapability, Description: "Ask for read access to " +
		"tables or views for a while. Agent governance checks the request; an operator " +
		"approves it in pg_sage (the result has the approval URL) and the grant then " +
		"lists only columns the database's environment allows. It expires on its own",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"capability":{"type":"string","enum":["read"]},` +
			`"objects":{"type":"array","minItems":1,"maxItems":50,"items":{"type":"string",` +
			`"minLength":3,"maxLength":300,"description":"schema.table"}},` +
			`"columns":{"type":"object","additionalProperties":{"type":"array",` +
			`"items":{"type":"string","maxLength":63}},"description":"object → columns; ` +
			`an object without an entry asks for every column the environment allows"},` +
			`"duration_minutes":{"type":"integer","minimum":1},` +
			`"reason":{"type":"string","minLength":1,"maxLength":2000}},` +
			`"required":["capability","objects","duration_minutes","reason"],` +
			`"additionalProperties":false}`)}}
}

type capabilityToolArgs struct {
	Database        string              `json:"database"`
	Capability      string              `json:"capability"`
	Objects         []string            `json:"objects"`
	Columns         map[string][]string `json:"columns"`
	DurationMinutes int                 `json:"duration_minutes"`
	Reason          string              `json:"reason"`
}

func (a capabilityToolArgs) request() (CapabilityRequest, *rpcError) {
	switch {
	case a.Capability == "":
		return CapabilityRequest{}, invalid("capability is required")
	case len(a.Objects) == 0 || len(a.Objects) > maxCapabilityObjects:
		return CapabilityRequest{}, invalid("objects must name 1-50 tables or views")
	case a.DurationMinutes < 1:
		return CapabilityRequest{}, invalid("duration_minutes must be at least 1")
	case a.Reason == "":
		return CapabilityRequest{}, invalid("reason is required")
	}
	named := map[string]bool{}
	out := CapabilityRequest{Database: a.Database, Capability: a.Capability,
		DurationMinutes: a.DurationMinutes, Reason: a.Reason}
	for _, o := range a.Objects {
		named[o] = true
		out.Objects = append(out.Objects, CapabilityObject{Object: o,
			Columns: a.Columns[o]})
	}
	for o := range a.Columns {
		if !named[o] {
			return CapabilityRequest{}, invalid("columns names " + o +
				", which is not in objects")
		}
	}
	return out, nil
}

func (s *Server) callGrantTool(ctx context.Context, _ string,
	arguments json.RawMessage) (any, *rpcError) {
	// An agent token without a principal (a migrated legacy token, the
	// unbound stdio client) reaches governance as unsponsored (G1-11).
	p, bound := PrincipalFromContext(ctx)
	if !bound || p.Kind != KindAgent {
		return nil, failure(codeNotPermitted, "agent_request_capability is for agent "+
			"principals; an operator grants access in pg_sage directly")
	}
	var args capabilityToolArgs
	if !decodeStrict(arguments, &args) {
		return nil, invalid("agent_request_capability takes capability, objects, columns, " +
			"duration_minutes and reason")
	}
	if db, ok := DatabaseFromContext(ctx); ok {
		args.Database = db
	}
	in, failed := args.request()
	if failed != nil {
		return nil, failed
	}
	backend, ok := s.backend.(GrantToolBackend)
	if !ok {
		return nil, failure(codeUnavailable, "agent grants are unavailable: agent "+
			"governance needs mode: meta or agents.control_database")
	}
	res, err := backend.RequestCapability(ctx, p.PrincipalID, in)
	if err != nil {
		return nil, grantFailure(err)
	}
	if res.Verdict == VerdictPark {
		return nil, failure(codeRateLimited, fmt.Sprintf("%s: retry after %d s",
			res.ReasonCode, res.RetryAfterSeconds))
	}
	return toolSuccess(res), nil
}

func grantFailure(err error) *rpcError {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(codeCancelled, "request cancelled")
	case errors.Is(err, agentguard.ErrInvalid):
		return failure(codeInvalidParams, "invalid arguments: "+err.Error())
	case errors.Is(err, envbind.ErrUnknownDatabase):
		return failure(codeUnknownDatabase, "unknown database")
	case errors.Is(err, agentguard.ErrNotFound):
		return failure(codeNotFound, err.Error())
	case errors.Is(err, agentguard.ErrUnavailable):
		return failure(codeUnavailable, err.Error())
	}
	return failure(codeInternal, "internal error")
}
