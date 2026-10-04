package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/facts"
)

// Binding facts (roadmap 2.3). list_facts reads a database's facts (any
// bound role); propose_fact records a fact for a person to confirm (it
// binds nothing until then); decide_fact confirms or rejects one (operator
// or admin). Confirmed facts only narrow or redirect what pg_sage does.

// FactRequest are the typed arguments of the fact tools.
type FactRequest struct {
	Database    string            `json:"database,omitempty"`
	Status      string            `json:"status,omitempty"`
	Type        string            `json:"type,omitempty"`
	SubjectKind string            `json:"subject_kind,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Value       map[string]string `json:"value,omitempty"`
	Evidence    string            `json:"evidence,omitempty"`
	FactID      int64             `json:"fact_id,omitempty"`
	Decision    string            `json:"decision,omitempty"`
	Note        string            `json:"note,omitempty"`
}

// FactBackend serves the fact tools. Writers get the bound principal's
// actor.
type FactBackend interface {
	ListFacts(context.Context, FactRequest) (any, error)
	ProposeFact(context.Context, FactRequest, string) (any, error)
	DecideFact(context.Context, FactRequest, string) (any, error)
}

var factToolNames = map[string]bool{"list_facts": true, "propose_fact": true,
	"decide_fact": true}

func factTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	types := `"type":{"type":"string","enum":["owned_by_app_migrations","test_fixture",` +
		`"slot_consumer","append_only","table_window"]}`
	return []Tool{
		{Name: "list_facts", Description: "List pg_sage's typed facts about a database " +
			"(who owns an object, test fixtures, CDC slots, archives, table windows): " +
			"proposed ones awaiting a person and confirmed ones, which bind pg_sage",
			InputSchema: schema(`{`+db+`,"status":{"type":"string"},`+types+`}`, `[]`)},
		{Name: "propose_fact", Description: "Propose a fact about the database with the " +
			"evidence for it. It binds nothing until a person confirms it. A confirmed " +
			"fact only narrows what pg_sage does (e.g. an index owned by the app's " +
			"migrations is changed through a PR, never by pg_sage's DDL)",
			InputSchema: schema(`{`+db+`,`+types+`,"subject_kind":{"type":"string",`+
				`"enum":["index","table","schema","slot"]},"subject":{"type":"string",`+
				`"maxLength":300},"value":{"type":"object"},"evidence":{"type":"string",`+
				`"maxLength":500}}`, `["type","subject_kind","subject","evidence"]`)},
		{Name: "decide_fact", Description: "Confirm or reject a proposed fact (or reject " +
			"a confirmed one). Confirmed facts only narrow or redirect what pg_sage does",
			InputSchema: schema(`{`+db+`,"fact_id":{"type":"integer","minimum":1},`+
				`"decision":{"type":"string","enum":["confirm","reject"]},`+
				`"note":{"type":"string","maxLength":1000}}`, `["fact_id","decision"]`)},
	}
}

func (s *Server) callFactTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(FactBackend)
	if !ok {
		return nil, failure(-32603, "facts unavailable")
	}
	var req FactRequest
	if !decodeStrict(raw, &req) || !req.valid(name) {
		return nil, failure(-32602, "invalid arguments")
	}
	var result any
	var err error
	switch name {
	case "list_facts":
		result, err = backend.ListFacts(ctx, req)
	case "propose_fact":
		result, err = backend.ProposeFact(ctx, req, ActorFromContext(ctx))
	default:
		result, err = backend.DecideFact(ctx, req, ActorFromContext(ctx))
	}
	if err != nil {
		return nil, factFailure(err)
	}
	return toolSuccess(result), nil
}

// valid checks a tool's required arguments and refuses another tool's.
func (r FactRequest) valid(tool string) bool {
	proposal := r.Type != "" || r.SubjectKind != "" || r.Subject != "" ||
		len(r.Value) > 0 || r.Evidence != ""
	decision := r.FactID != 0 || r.Decision != "" || r.Note != ""
	switch tool {
	case "list_facts":
		return !decision && r.SubjectKind == "" && r.Subject == "" && r.Evidence == ""
	case "propose_fact":
		return !decision && r.Status == "" && r.Type != "" && r.SubjectKind != "" &&
			r.Subject != "" && r.Evidence != ""
	default:
		return !proposal && r.Status == "" && r.FactID > 0 &&
			(r.Decision == "confirm" || r.Decision == "reject")
	}
}

func factFailure(err error) *rpcError {
	switch {
	case errors.Is(err, facts.ErrNotFound):
		return failure(-32004, "not found")
	case errors.Is(err, facts.ErrInvalidType), errors.Is(err, facts.ErrInvalidKind),
		errors.Is(err, facts.ErrInvalidSubject), errors.Is(err, facts.ErrProtectedSubject),
		errors.Is(err, facts.ErrInvalidValue), errors.Is(err, facts.ErrNoEvidence),
		errors.Is(err, facts.ErrInvalidSource), errors.Is(err, facts.ErrInvalidTransition),
		errors.Is(err, facts.ErrChanged):
		return failure(-32602, "invalid arguments: "+err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(-32800, "request cancelled")
	}
	return failure(-32603, "internal error")
}
