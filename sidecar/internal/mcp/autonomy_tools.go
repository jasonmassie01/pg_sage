package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Sage SRE M7 autonomy tools (AI-SRE-SPEC §9). sre_get_autonomy reads the
// earned-autonomy ledger (any bound role); sre_downgrade_autonomy steps
// autonomy down (operator), which is always a restriction. There is no
// approval tool: a promotion takes a human's approval in the UI/API.

// AutonomyRequest are the typed arguments of the autonomy tools.
type AutonomyRequest struct {
	Database    string `json:"database,omitempty"`
	Family      string `json:"family,omitempty"`
	ActionClass string `json:"action_class,omitempty"`
	Level       string `json:"level,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// AutonomyBackend serves the autonomy tools.
type AutonomyBackend interface {
	GetAutonomy(context.Context, AutonomyRequest) (any, error)
	DowngradeAutonomy(context.Context, AutonomyRequest, string) (any, error)
}

var autonomyToolNames = map[string]bool{"sre_get_autonomy": true,
	"sre_downgrade_autonomy": true}

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
	if name == "sre_get_autonomy" {
		result, err = backend.GetAutonomy(ctx, req)
	} else {
		result, err = backend.DowngradeAutonomy(ctx, req, ActorFromContext(ctx))
	}
	if err != nil {
		return nil, autonomyFailure(err)
	}
	return map[string]any{"structuredContent": result}, nil
}

func (r AutonomyRequest) valid(tool string) bool {
	if tool == "sre_get_autonomy" {
		return true
	}
	return r.Family != "" && r.ActionClass != "" && r.Level != "" && r.Reason != ""
}

func autonomyFailure(err error) *rpcError {
	switch {
	case errors.Is(err, earned.ErrInvalidRequest), errors.Is(err, earned.ErrNotADowngrade):
		return failure(-32602, "invalid arguments: "+err.Error())
	case errors.Is(err, earned.ErrNotFound):
		return failure(-32004, "not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(-32800, "request cancelled")
	}
	return failure(-32603, "internal error")
}
