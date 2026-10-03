package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE read tools (AI-SRE-SPEC §9). They are read-only (any bound
// role), take only typed identifiers (never SQL), and return redacted,
// evidence-bundled data. Starting investigations and proposals are later
// milestones; nothing here executes.

var sreToolNames = map[string]bool{"sre_list_incidents": true,
	"sre_get_investigation": true, "sre_get_evidence": true}

func sreTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	inv := `"investigation_id":{"type":"string","format":"uuid"}`
	return []Tool{
		{Name: "sre_list_incidents", Description: "List Sage SRE investigations of " +
			"RCA incidents and plan regressions: family, state, case and conclusion",
			InputSchema: schema(`{`+db+`,"case_id":{"type":"string"}}`, `[]`)},
		{Name: "sre_get_investigation", Description: "Read one investigation: " +
			"observed facts, likely explanation, alternatives, ruled-out " +
			"hypotheses with evidence, missing evidence and operator steps",
			InputSchema: schema(`{`+db+`,`+inv+`}`, `["investigation_id"]`)},
		{Name: "sre_get_evidence", Description: "Read one typed, redacted evidence " +
			"item of an investigation",
			InputSchema: schema(`{`+db+`,`+inv+`,"evidence_id":{"type":"string",`+
				`"format":"uuid"}}`, `["investigation_id","evidence_id"]`)},
	}
}

func (s *Server) callSRETool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	if _, ok := runbookToolNames[name]; ok {
		return s.callRunbookTool(ctx, name, raw)
	}
	backend, ok := s.backend.(InvestigationBackend)
	if !ok {
		return nil, failure(-32603, "investigations unavailable")
	}
	var req InvestigationRequest
	if !decodeStrict(raw, &req) || !req.valid(name) {
		return nil, failure(-32602, "invalid arguments")
	}
	var result any
	var err error
	switch name {
	case "sre_list_incidents":
		result, err = backend.ListInvestigations(ctx, req)
	case "sre_get_investigation":
		result, err = backend.GetInvestigation(ctx, req)
	default:
		result, err = backend.GetEvidence(ctx, req)
	}
	if err != nil {
		return nil, sreFailure(err)
	}
	return toolSuccess(result), nil
}

func (r InvestigationRequest) valid(tool string) bool {
	switch tool {
	case "sre_get_investigation":
		return r.InvestigationID != ""
	case "sre_get_evidence":
		return r.InvestigationID != "" && r.EvidenceID != ""
	}
	return true
}

// decodeStrict rejects unknown argument names.
func decodeStrict(raw json.RawMessage, target any) bool {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(target) == nil
}

func sreFailure(err error) *rpcError {
	switch {
	case errors.Is(err, sre.ErrNotFound):
		return failure(-32004, "not found")
	case errors.Is(err, sre.ErrInvalidRequest):
		return failure(-32602, "invalid arguments")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(-32800, "request cancelled")
	case errors.Is(err, sre.ErrMetadataUnavailable):
		return failure(-32603, "investigation store unavailable")
	}
	return failure(-32603, "internal error")
}
