package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"
)

// Sage SRE M5 read tools (AI-SRE-SPEC §8/§9): SLO error-budget state
// (with the SLI recovery predicate) and the change feed. Read-only (any
// bound role), typed arguments only, nothing executes.

var signalToolNames = map[string]bool{"sre_list_slos": true, "sre_get_slo": true,
	"sre_list_changes": true}

// Change-feed window bounds (minutes).
const (
	defaultChangeWindow = 60
	maxChangeWindow     = 7 * 24 * 60
)

// SignalRequest are the typed arguments of the SLO and change-feed tools.
type SignalRequest struct {
	Database      string `json:"database,omitempty"`
	Name          string `json:"name,omitempty"`
	RecoverySince string `json:"recovery_since,omitempty"`
	WindowMinutes int    `json:"window_minutes,omitempty"`
}

// SignalBackend serves the SLO and change-feed tools.
type SignalBackend interface {
	ListSLOs(context.Context, SignalRequest) (any, error)
	GetSLO(context.Context, SignalRequest) (any, error)
	ListChanges(context.Context, SignalRequest) (any, error)
}

func signalTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	return []Tool{
		{Name: "sre_list_slos", Description: "List SLO error-budget states: ok, ticket, " +
			"page or unknown (with why), burn rates per window, budget left; app SLIs " +
			"claim customer impact, database proxies never do",
			InputSchema: schema(`{`+db+`}`, `[]`)},
		{Name: "sre_get_slo", Description: "Read one SLO's state and state history; with " +
			"recovery_since, the SLI recovery verdict (never certified on missing data, " +
			"counter resets or low traffic)",
			InputSchema: schema(`{`+db+`,"name":{"type":"string"},`+
				`"recovery_since":{"type":"string","format":"date-time"}}`, `["name"]`)},
		{Name: "sre_list_changes", Description: "List what changed around a database: " +
			"signed deploys, migrations and feature flags, pg_sage's own actions, config " +
			"changes, DDL, statistics resets, restarts, failovers and extension changes",
			InputSchema: schema(`{`+db+`,"window_minutes":{"type":"integer",`+
				`"minimum":1,"maximum":10080}}`, `[]`)},
	}
}

func (s *Server) callSignalTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(SignalBackend)
	if !ok {
		return nil, failure(-32603, "SLOs and the change feed are unavailable")
	}
	var req SignalRequest
	if !decodeStrict(raw, &req) || !req.valid(name) {
		return nil, failure(-32602, "invalid arguments")
	}
	if req.WindowMinutes == 0 {
		req.WindowMinutes = defaultChangeWindow
	}
	var result any
	var err error
	switch name {
	case "sre_list_slos":
		result, err = backend.ListSLOs(ctx, req)
	case "sre_get_slo":
		result, err = backend.GetSLO(ctx, req)
	default:
		result, err = backend.ListChanges(ctx, req)
	}
	if err != nil {
		return nil, sreFailure(err)
	}
	return toolSuccess(result), nil
}

func (r SignalRequest) valid(tool string) bool {
	if r.WindowMinutes < 0 || r.WindowMinutes > maxChangeWindow || !plainText(r.Name) ||
		!plainText(r.Database) {
		return false
	}
	if r.RecoverySince != "" {
		if _, err := time.Parse(time.RFC3339, r.RecoverySince); err != nil {
			return false
		}
	}
	return tool != "sre_get_slo" || r.Name != ""
}

func plainText(s string) bool {
	return len(s) <= 128 && strings.IndexFunc(s, unicode.IsControl) < 0
}
