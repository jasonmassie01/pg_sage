package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// get_outcomes (ux-02a): the outcome ledger, read-only, the same payload
// as GET /api/v1/outcomes. No DB here: the read is tested in
// internal/outcomes and, against the route, in internal/api.

type outcomesRecordingBackend struct {
	recordingBackend
	calls int
	last  OutcomesRequest
	err   error
}

func (b *outcomesRecordingBackend) Outcomes(_ context.Context,
	req OutcomesRequest) (any, error) {
	b.calls++
	b.last = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"rows": []any{}, "summary": map[string]any{"changes": 0}}, nil
}

func outcomesTool(t *testing.T) Tool {
	t.Helper()
	for _, tool := range NewServer(&outcomesRecordingBackend{}).Tools() {
		if tool.Name == toolGetOutcomes {
			return tool
		}
	}
	t.Fatal("get_outcomes is not listed by tools/list")
	return Tool{}
}

func TestOutcomesToolIsListedReadOnly(t *testing.T) {
	tool := outcomesTool(t)
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
	require.True(t, strings.Contains(tool.Description, "what pg_sage changed"),
		tool.Description)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional bool                       `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, 5, len(schema.Properties), "database, since, verdict, evidence, limit")
	for _, name := range []string{"database", "since", "verdict", "evidence", "limit"} {
		_, ok := schema.Properties[name]
		require.True(t, ok, name)
	}
	require.True(t, !schema.Additional, "additionalProperties false")
	require.True(t, strings.Contains(string(schema.Properties["verdict"]), `"no_change"`),
		"verdict enumerates the ledger verdicts")
	require.True(t, strings.Contains(string(schema.Properties["database"]),
		"every database you may read"), "database is optional")
	scope, known := RequiredScope(toolGetOutcomes, nil)
	require.True(t, known && scope == ScopeRead, "read scope")
}

func TestOutcomesToolPassesArguments(t *testing.T) {
	backend := &outcomesRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	response := invoke(t, NewServer(backend), viewer, toolCall(toolGetOutcomes,
		`{"database":"orders","since":"168h","verdict":"regressed","evidence":"ev-1",`+
			`"limit":5}`))
	require.Equal(t, 0, response.Error.Code, "a viewer may read the ledger")
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.last.Database)
	require.Equal(t, "168h0m0s", backend.last.Since.String())
	require.Equal(t, "regressed", backend.last.Verdict)
	require.Equal(t, "ev-1", backend.last.Evidence)
	require.Equal(t, 5, backend.last.Limit)
	require.True(t, backend.last.Databases == nil, "unrestricted principal: every database")
	response = invoke(t, NewServer(backend), viewer, toolCall(toolGetOutcomes, `{}`))
	require.Equal(t, 0, response.Error.Code, "no arguments: the default window")
	require.Equal(t, "24h0m0s", backend.last.Since.String())
	require.Equal(t, 500, backend.last.Limit)
}

func TestOutcomesToolRefusesBadArguments(t *testing.T) {
	backend := &outcomesRecordingBackend{}
	for _, args := range []string{`{"since":"abc"}`, `{"since":"-1h"}`, `{"since":"9601h"}`,
		`{"verdict":"bogus"}`, `{"limit":0}`, `{"limit":10001}`, `{"other":1}`,
		`{"database":"../x"}`, `{"since":24}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolGetOutcomes, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls, "refusals never reach the backend")
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetOutcomes, `{"since":"9600h"}`))
	require.Equal(t, 0, response.Error.Code, "9600h is the bound, inclusive")
}

func TestOutcomesToolScopesAndErrors(t *testing.T) {
	backend := &outcomesRecordingBackend{}
	agent := WithPrincipal(context.Background(), Principal{Actor: "agent:a", Role: "viewer",
		Kind: KindAgent, Scopes: []Scope{ScopeRead}, Databases: []string{"tenant_a"}})
	response := invoke(t, NewServer(backend), agent, toolCall(toolGetOutcomes, `{}`))
	require.Equal(t, 0, response.Error.Code, "an agent may read")
	require.Equal(t, "tenant_a", strings.Join(backend.last.Databases, ","))
	backend.err = ErrOutcomesDatabaseNotFound
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetOutcomes, `{"database":"other"}`))
	require.Equal(t, codeNotFound, response.Error.Code, "unknown or hidden database")
	backend.err = errors.New("read failed")
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetOutcomes, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolGetOutcomes, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).Outcomes(context.Background(), OutcomesRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "no dependency")
}
