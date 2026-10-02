package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/packetreview"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE M7 autonomy tools (AI-SRE-SPEC §9). sre_get_autonomy reads the
// earned-autonomy ledger (any bound role); sre_downgrade_autonomy steps
// autonomy down (operator), which is always a restriction. Phase 1.1
// (2026-10-02): sre_review_investigation records an operator's review of
// a finished investigation (the shadow review and the investigation
// outcome) and sre_evaluate_autonomy asks pg_sage to propose what the
// evidence supports, explaining what it did not (both operator). There
// is no approval tool: a promotion takes a human's approval in the UI/API.

// AutonomyRequest are the typed arguments of the autonomy tools.
type AutonomyRequest struct {
	Database        string `json:"database,omitempty"`
	Family          string `json:"family,omitempty"`
	ActionClass     string `json:"action_class,omitempty"`
	Level           string `json:"level,omitempty"`
	Reason          string `json:"reason,omitempty"`
	InvestigationID string `json:"investigation_id,omitempty"`
	Verdict         string `json:"verdict,omitempty"`
	Note            string `json:"note,omitempty"`
	ActualRootCause string `json:"actual_root_cause,omitempty"`
}

// AutonomyBackend serves the autonomy tools. Writers get the bound
// principal's actor.
type AutonomyBackend interface {
	GetAutonomy(context.Context, AutonomyRequest) (any, error)
	DowngradeAutonomy(context.Context, AutonomyRequest, string) (any, error)
	ReviewInvestigation(context.Context, AutonomyRequest, string) (any, error)
	EvaluateAutonomy(context.Context, AutonomyRequest) (any, error)
}

var autonomyToolNames = map[string]bool{"sre_get_autonomy": true,
	"sre_downgrade_autonomy": true, "sre_review_investigation": true,
	"sre_evaluate_autonomy": true}

func autonomyTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	return []Tool{
		{Name: "sre_get_autonomy", Description: "Read pg_sage's earned autonomy: the " +
			"level per incident family and action class (L0-L3), its cap, the level " +
			"its evidence supports, active downgrades, pending promotions and history",
			InputSchema: schema(`{`+db+`}`, `[]`)},
		{Name: "sre_downgrade_autonomy", Description: "Lower pg_sage's autonomy for a " +
			"family and action class (\"*\" for every class of the family). Restricting " +
			"is always allowed; raising autonomy needs a human approval in pg_sage",
			InputSchema: schema(`{`+db+`,"family":{"type":"string"},`+
				`"action_class":{"type":"string"},"level":{"type":"string",`+
				`"enum":["L0","L1","L2"]},"reason":{"type":"string","maxLength":1000}}`,
				`["family","action_class","level","reason"]`)},
		{Name: "sre_review_investigation", Description: "Review a concluded or " +
			"inconclusive Sage SRE investigation: accept or reject its diagnosis, " +
			"optionally with a note and the actual root cause (a causal-graph node id " +
			"or free text). Records the shadow review that earned autonomy counts and " +
			"the investigation outcome. It never approves a promotion",
			InputSchema: schema(`{`+db+`,"investigation_id":{"type":"string"},`+
				`"verdict":{"type":"string","enum":["accepted","rejected"]},`+
				`"note":{"type":"string","maxLength":1500},`+
				`"actual_root_cause":{"type":"string","maxLength":400}}`,
				`["investigation_id","verdict"]`)},
		{Name: "sre_evaluate_autonomy", Description: "Ask pg_sage to propose the " +
			"promotions its evidence supports now; returns the proposals created and, " +
			"for every other family and action class, why not (each unmet check with " +
			"how to meet it). An admin still approves every promotion",
			InputSchema: schema(`{`+db+`}`, `[]`)},
	}
}

func (s *Server) callAutonomyTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(AutonomyBackend)
	if !ok {
		return nil, failure(-32603, "autonomy unavailable")
	}
	var req AutonomyRequest
	if !decodeStrict(raw, &req) || !req.valid(name) {
		return nil, failure(-32602, "invalid arguments")
	}
	var result any
	var err error
	switch name {
	case "sre_get_autonomy":
		result, err = backend.GetAutonomy(ctx, req)
	case "sre_review_investigation":
		result, err = backend.ReviewInvestigation(ctx, req, ActorFromContext(ctx))
	case "sre_evaluate_autonomy":
		result, err = backend.EvaluateAutonomy(ctx, req)
	default:
		result, err = backend.DowngradeAutonomy(ctx, req, ActorFromContext(ctx))
	}
	if err != nil {
		return nil, autonomyFailure(err)
	}
	return map[string]any{"structuredContent": result}, nil
}

// valid checks a tool's required arguments and refuses another tool's.
func (r AutonomyRequest) valid(tool string) bool {
	downgrade := r.Family != "" || r.ActionClass != "" || r.Level != "" || r.Reason != ""
	review := r.InvestigationID != "" || r.Verdict != "" || r.Note != "" ||
		r.ActualRootCause != ""
	switch tool {
	case "sre_get_autonomy", "sre_evaluate_autonomy":
		return !downgrade && !review
	case "sre_review_investigation":
		return !downgrade && r.InvestigationID != "" &&
			(r.Verdict == "accepted" || r.Verdict == "rejected")
	}
	return !review && r.Family != "" && r.ActionClass != "" && r.Level != "" &&
		r.Reason != ""
}

func autonomyFailure(err error) *rpcError {
	switch {
	case errors.Is(err, earned.ErrInvalidRequest), errors.Is(err, earned.ErrNotADowngrade),
		errors.Is(err, packetreview.ErrNotFinished):
		return failure(-32602, "invalid arguments: "+err.Error())
	case errors.Is(err, earned.ErrNotFound), errors.Is(err, sre.ErrNotFound):
		return failure(-32004, "not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(-32800, "request cancelled")
	}
	return failure(-32603, "internal error")
}
