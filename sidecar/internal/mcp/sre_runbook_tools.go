package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Sage SRE runbook and incident memory tools (AI-SRE-SPEC §7.1, §9).
// Viewers read runbooks, run history and similar past incidents;
// operators write drafts and compile English into drafts. No tool signs a
// runbook: a signature authorizes future probe plans and is a human act
// through the authenticated UI/API, never something an agent does.

// RunbookRequest are the typed arguments of the runbook tools.
type RunbookRequest struct {
	Database        string          `json:"database,omitempty"`
	RunbookID       string          `json:"runbook_id,omitempty"`
	BaseVersion     int             `json:"base_version,omitempty"`
	Definition      json.RawMessage `json:"definition,omitempty"`
	Text            string          `json:"text,omitempty"`
	InvestigationID string          `json:"investigation_id,omitempty"`
}

// RunbookBackend serves the runbook and memory tools.
type RunbookBackend interface {
	ListRunbooks(context.Context, RunbookRequest) (any, error)
	GetRunbook(context.Context, RunbookRequest) (any, error)
	RunbookRuns(context.Context, RunbookRequest) (any, error)
	DraftRunbook(context.Context, RunbookRequest) (any, error)
	CompileRunbook(context.Context, RunbookRequest) (any, error)
	SimilarIncidents(context.Context, RunbookRequest) (any, error)
}

// runbookToolNames maps each tool to whether it writes (operator).
var runbookToolNames = map[string]bool{"sre_list_runbooks": false,
	"sre_get_runbook": false, "sre_runbook_runs": false, "sre_similar_incidents": false,
	"sre_draft_runbook": true, "sre_compile_runbook": true}

func runbookTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` +
			`"database":{"type":"string","description":"fleet database name"}` +
			properties + `},"required":` + required + `,"additionalProperties":false}`)
	}
	rb := `,"runbook_id":{"type":"string","format":"uuid"}`
	return []Tool{
		{Name: "sre_list_runbooks", Description: "List a database's typed runbooks: " +
			"status (draft, signed, retired, invalid), whether each runs, latest version",
			InputSchema: schema(``, `[]`)},
		{Name: "sre_get_runbook", Description: "Read one runbook: every version's DAG, " +
			"content hash, signer and imported playbook text",
			InputSchema: schema(rb, `["runbook_id"]`)},
		{Name: "sre_runbook_runs", Description: "List the investigations a runbook ran " +
			"in, with the version, path and proposal of each run",
			InputSchema: schema(rb, `["runbook_id"]`)},
		{Name: "sre_similar_incidents", Description: "List similar past incidents of " +
			"an investigation and their operator-verified outcomes (context, not evidence)",
			InputSchema: schema(`,"investigation_id":{"type":"string","format":"uuid"}`,
				`["investigation_id"]`)},
		{Name: "sre_draft_runbook", Description: "Create a runbook draft from a typed " +
			"definition, or add a draft version (runbook_id and base_version). Drafts " +
			"never run until an admin signs them in pg_sage",
			InputSchema: schema(rb+`,"base_version":{"type":"integer","minimum":1},`+
				`"definition":{"type":"object"}`, `["definition"]`)},
		{Name: "sre_compile_runbook", Description: "Compile an English playbook into a " +
			"runbook draft with pg_sage's model; it never runs until an admin signs it",
			InputSchema: schema(`,"text":{"type":"string","maxLength":16000}`, `["text"]`)},
	}
}

func (s *Server) callRunbookTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	if runbookToolNames[name] && !canMutate(ctx) {
		return nil, failure(-32001, "operator or admin role required")
	}
	backend, ok := s.backend.(RunbookBackend)
	if !ok {
		return nil, failure(-32603, "runbooks unavailable")
	}
	var req RunbookRequest
	if !decodeStrict(raw, &req) || !req.valid(name) {
		return nil, failure(-32602, "invalid arguments")
	}
	calls := map[string]func(context.Context, RunbookRequest) (any, error){
		"sre_list_runbooks": backend.ListRunbooks, "sre_get_runbook": backend.GetRunbook,
		"sre_runbook_runs": backend.RunbookRuns, "sre_draft_runbook": backend.DraftRunbook,
		"sre_compile_runbook":   backend.CompileRunbook,
		"sre_similar_incidents": backend.SimilarIncidents,
	}
	result, err := calls[name](ctx, req)
	if err != nil {
		return nil, runbookFailure(err)
	}
	return toolSuccess(result), nil
}

func (r RunbookRequest) valid(tool string) bool {
	switch tool {
	case "sre_get_runbook", "sre_runbook_runs":
		return r.RunbookID != ""
	case "sre_similar_incidents":
		return r.InvestigationID != ""
	case "sre_draft_runbook":
		return len(r.Definition) > 0 && (r.RunbookID == "") == (r.BaseVersion == 0)
	case "sre_compile_runbook":
		return strings.TrimSpace(r.Text) != ""
	}
	return true
}

func runbookFailure(err error) *rpcError {
	var rej *runbook.Rejection
	switch {
	case errors.As(err, &rej):
		return failure(-32022, "runbook draft rejected: "+rej.Reason)
	case errors.Is(err, sre.ErrModelUnavailable):
		return failure(-32010, "no model is available to compile a playbook")
	case errors.Is(err, sre.ErrVersionConflict), errors.Is(err, sre.ErrHashMismatch),
		errors.Is(err, sre.ErrAlreadySigned), errors.Is(err, sre.ErrRetired):
		return failure(-32009, "conflict: "+err.Error())
	}
	return sreFailure(err)
}
