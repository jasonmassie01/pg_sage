package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// get_request_status and wait_for_decision (mcp-01b) take the handle a
// held result carries ("<kind>:<id>", held_result.go) and report the item's
// state: {handle, database, state, decided_by_kind, outcome, next}. The
// status is the requester's own: another principal's handle, or a caller
// with no principal, gets not_found, the same answer as a handle that does
// not exist. Both tools only read; neither decides anything.

const (
	toolGetRequestStatus = "get_request_status"
	toolWaitForDecision  = "wait_for_decision"
)

var requestStatusToolNames = map[string]bool{toolGetRequestStatus: true,
	toolWaitForDecision: true}

// Handle kinds: the inbox kinds a held result names, plus a ledger
// decision the policy check withheld ("change:<evidence_id>").
var handleKinds = map[string]bool{"change_approval": true, "plan": true,
	"agent_request": true, "policy_exception": true, "change": true}

// evidenceIDPattern bounds a ledger evidence id in a change handle.
var evidenceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// RequestQuery names one held item on one database.
type RequestQuery struct {
	Database string
	Kind     string
	ID       string
}

// RequestRecord is a held item's state as its store has it now. Final
// says nothing more will happen to it; the owner fields say who made the
// request (an agent principal id, or the MCP actor that proposed it).
type RequestRecord struct {
	State            string
	DecidedByKind    string // "", person, policy or system
	Outcome          string
	Final            bool
	OwnerPrincipalID string
	OwnerActor       string
}

// RequestStatusBackend reads a held item's state.
type RequestStatusBackend interface {
	RequestStatus(context.Context, RequestQuery) (RequestRecord, error)
}

// ErrRequestNotFound is a handle with no item behind it.
var ErrRequestNotFound = errors.New("request not found")

func requestStatusTools() []Tool {
	handle := `"handle":{"type":"string","maxLength":160,` +
		`"pattern":"^(change_approval|plan|agent_request|policy_exception|change):[^\\s]+$",` +
		`"description":"the handle a held result returned, such as change_approval:12"}`
	return []Tool{
		{Name: toolGetRequestStatus, InputSchema: json.RawMessage(`{"type":"object",` +
			`"properties":{` + handle + `},"required":["handle"],` +
			`"additionalProperties":false}`)},
		{Name: toolWaitForDecision, InputSchema: json.RawMessage(`{"type":"object",` +
			`"properties":{` + handle + `,"timeout_seconds":{"type":"integer",` +
			`"minimum":1,"maximum":60,"description":"how long to wait (default 30)"}},` +
			`"required":["handle"],"additionalProperties":false}`)},
	}
}

// statusArgs are the typed arguments of both tools.
type statusArgs struct {
	Database string          `json:"database"`
	Handle   json.RawMessage `json:"handle"`
	Timeout  json.RawMessage `json:"timeout_seconds"`
}

// parseHandle reads "<kind>:<id>": a known kind, a positive int64 id, or
// an evidence id for a change.
func parseHandle(raw json.RawMessage) (string, string, bool) {
	var handle string
	if json.Unmarshal(raw, &handle) != nil {
		return "", "", false
	}
	kind, id, ok := strings.Cut(handle, ":")
	if !ok || !handleKinds[kind] {
		return "", "", false
	}
	if kind == "change" {
		return kind, id, evidenceIDPattern.MatchString(id)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	return kind, id, err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

func invalidHandle(tool string) *rpcError {
	return failure(codeInvalidParams, "invalid arguments: "+tool+" takes a handle "+
		"\"<kind>:<id>\" (kind change_approval, plan, agent_request, policy_exception "+
		"or change) as a held result returned it")
}

func (s *Server) callRequestStatusTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	var args statusArgs
	if !decodeStrict(raw, &args) {
		return nil, invalidHandle(name)
	}
	kind, id, ok := parseHandle(args.Handle)
	if !ok {
		return nil, invalidHandle(name)
	}
	query := RequestQuery{Database: args.Database, Kind: kind, ID: id}
	if name == toolWaitForDecision {
		timeout, ok := waitTimeout(args.Timeout)
		if !ok {
			return nil, failure(codeInvalidParams, "invalid arguments: "+
				"timeout_seconds is a whole number from 1 to 60")
		}
		return s.waitForDecision(ctx, query, timeout)
	}
	record, failed := s.ownedStatus(ctx, query)
	if failed != nil {
		return nil, failed
	}
	return statusResult(query, record), nil
}

// ownedStatus reads the item and hides it from anyone but its requester.
func (s *Server) ownedStatus(ctx context.Context, q RequestQuery) (RequestRecord,
	*rpcError) {
	backend, ok := s.backend.(RequestStatusBackend)
	if !ok {
		return RequestRecord{}, failure(codeUnavailable, "request status unavailable")
	}
	record, err := backend.RequestStatus(ctx, q)
	switch {
	case errors.Is(err, ErrRequestNotFound):
		return RequestRecord{}, notFoundHandle(q)
	case err != nil:
		return RequestRecord{}, failure(codeUnavailable, "could not read the status of "+
			handleOf(q)+" now")
	case !ownsRequest(ctx, record):
		return RequestRecord{}, notFoundHandle(q)
	}
	return record, nil
}

func notFoundHandle(q RequestQuery) *rpcError {
	return failure(codeNotFound, "request "+handleOf(q)+" not found")
}

func handleOf(q RequestQuery) string { return q.Kind + ":" + q.ID }

// ownsRequest: the bound principal made the request. It fails closed: an
// unbound caller, or an item that names no owner, matches nobody.
func ownsRequest(ctx context.Context, record RequestRecord) bool {
	p, bound := PrincipalFromContext(ctx)
	if !bound {
		return false
	}
	if id := strings.TrimSpace(p.PrincipalID); id != "" && record.OwnerPrincipalID == id {
		return true
	}
	actor := strings.TrimSpace(p.Actor)
	return actor != "" && record.OwnerActor != "" && record.OwnerActor == "mcp:"+actor
}

// statusResult is the answer both tools give for record.
func statusResult(q RequestQuery, record RequestRecord) map[string]any {
	args := map[string]any{"handle": handleOf(q)}
	if q.Database != "" {
		args["database"] = q.Database
	}
	var next any
	switch {
	case record.State == stateUnknown:
		next = map[string]any{"tool": toolGetRequestStatus, "arguments": args}
	case !record.Final:
		next = map[string]any{"tool": toolWaitForDecision, "arguments": args}
	}
	return toolSuccess(map[string]any{"handle": handleOf(q), "database": q.Database,
		"state": record.State, "decided_by_kind": record.DecidedByKind,
		"outcome": record.Outcome, "next": next})
}
