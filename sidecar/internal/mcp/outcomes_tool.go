package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/outcomes"
)

// get_outcomes (ux-02a) is the outcome ledger: one row per change pg_sage
// made in a window, with what, why, expected, measured, verdict and
// rollback state; the same payload as GET /api/v1/outcomes. Read-only.
// get_ledger reads policy decisions, not outcomes.

const toolGetOutcomes = "get_outcomes"

// OutcomesRequest are get_outcomes' typed arguments plus the databases the
// caller may see (nil: all).
type OutcomesRequest struct {
	Database  string        `json:"database,omitempty"`
	Since     time.Duration `json:"since,omitempty"`
	Verdict   string        `json:"verdict,omitempty"`
	Evidence  string        `json:"evidence,omitempty"`
	Limit     int           `json:"limit,omitempty"`
	Databases []string      `json:"-"`
}

// OutcomesBackend serves get_outcomes.
type OutcomesBackend interface {
	Outcomes(context.Context, OutcomesRequest) (any, error)
}

// ErrOutcomesDatabaseNotFound is a get_outcomes database that does not
// exist or that the caller may not read: one answer for both.
var ErrOutcomesDatabaseNotFound = errors.New("database not found")

func outcomesTools() []Tool {
	verdicts := []string{}
	for _, v := range outcomes.Verdicts() {
		verdicts = append(verdicts, fmt.Sprintf("%q", v))
	}
	return []Tool{{Name: toolGetOutcomes,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"since":{"type":"string","description":"window as a duration from 1s to ` +
			`9600h (default 24h), such as 24h or 168h"},` +
			`"verdict":{"type":"string","enum":[` + strings.Join(verdicts, ",") + `]},` +
			`"evidence":{"type":"string","maxLength":200,"description":"only the changes ` +
			`a decision or credited incident evidence id cites"},` +
			`"limit":{"type":"integer","minimum":1,"maximum":10000,` +
			`"description":"rows returned (default 500); the summary covers the window"}},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callOutcomesTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	req, failed := parseOutcomesArgs(raw)
	if failed != nil {
		return nil, failed
	}
	if p, bound := PrincipalFromContext(ctx); bound && p.Databases != nil {
		req.Databases = append([]string{}, p.Databases...)
	}
	backend, ok := s.backend.(OutcomesBackend)
	if !ok {
		return nil, failure(codeInternal, "outcome ledger unavailable")
	}
	result, err := backend.Outcomes(ctx, req)
	if errors.Is(err, ErrOutcomesDatabaseNotFound) {
		return nil, failure(codeNotFound, "database not found")
	}
	return toolResult(result, err)
}

func parseOutcomesArgs(raw json.RawMessage) (OutcomesRequest, *rpcError) {
	var args struct {
		Database string `json:"database"`
		Since    string `json:"since"`
		Verdict  string `json:"verdict"`
		Evidence string `json:"evidence"`
		Limit    *int   `json:"limit"`
	}
	if !decodeStrict(raw, &args) {
		return OutcomesRequest{}, failure(codeInvalidParams, "invalid arguments: "+
			"get_outcomes takes only database, since, verdict, evidence and limit")
	}
	since, err := outcomes.ParseSince(args.Since)
	if err == nil {
		_, err = outcomes.ParseVerdict(args.Verdict)
	}
	if err == nil && args.Limit != nil && (*args.Limit < 1 || *args.Limit > outcomes.MaxLimit) {
		err = fmt.Errorf("limit must be an integer from 1 to %d", outcomes.MaxLimit)
	}
	if err == nil && len(args.Evidence) > 200 {
		err = errors.New("evidence is at most 200 characters")
	}
	if err == nil && args.Database != "" && validateTargetDatabase(args.Database) != nil {
		err = errors.New("invalid database name")
	}
	if err != nil {
		return OutcomesRequest{}, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	}
	req := OutcomesRequest{Database: args.Database, Since: since, Verdict: args.Verdict,
		Evidence: args.Evidence, Limit: outcomes.DefaultLimit}
	if args.Limit != nil {
		req.Limit = *args.Limit
	}
	return req, nil
}
