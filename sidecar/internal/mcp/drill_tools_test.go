package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// restore_drills (drill-01d): "can this database be restored", read-only,
// the latest drill result per database in the caller's scope. No DB here:
// the read is tested in internal/drill and, through the same backend as
// the route, in internal/api (TestRestoreDrillsMCPDB_*).
// No concurrent access tests: the tool keeps no state of its own; the
// backend's reads are concurrent-tested in internal/api.

type drillRecordingBackend struct {
	recordingBackend
	calls int
	last  RestoreDrillsRequest
	err   error
}

func (b *drillRecordingBackend) RestoreDrills(_ context.Context,
	req RestoreDrillsRequest) (any, error) {
	b.calls++
	b.last = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"total": 1, "passed": 1, "databases": []any{}}, nil
}

func drillTool(t *testing.T) Tool {
	t.Helper()
	for _, tool := range NewServer(&drillRecordingBackend{}).Tools() {
		if tool.Name == toolRestoreDrills {
			return tool
		}
	}
	t.Fatal("restore_drills is not listed by tools/list")
	return Tool{}
}

func TestDrillToolsListedReadOnlyWithAnOptionalDatabase(t *testing.T) {
	tool := drillTool(t)
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
	require.True(t, strings.Contains(tool.Description, "restore drill"), tool.Description)
	require.True(t, strings.Contains(tool.Description, "Read-only"), tool.Description)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional bool                       `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, 1, len(schema.Properties), "database only")
	require.True(t, strings.Contains(string(schema.Properties["database"]),
		"every database you may read"), "database is optional")
	require.True(t, !schema.Additional, "additionalProperties false")
	scope, known := RequiredScope(toolRestoreDrills, nil)
	require.True(t, known && scope == ScopeRead, "read scope")
}

func TestDrillToolsPassTheDatabaseAndTheScope(t *testing.T) {
	backend := &drillRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	response := invoke(t, NewServer(backend), viewer, toolCall(toolRestoreDrills,
		`{"database":"orders"}`))
	require.Equal(t, 0, response.Error.Code, "a viewer may read")
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.last.Database)
	require.True(t, backend.last.Databases == nil, "unrestricted principal: every database")
	response = invoke(t, NewServer(backend), viewer, toolCall(toolRestoreDrills, `{}`))
	require.Equal(t, 0, response.Error.Code, "no arguments: every database")
	require.Equal(t, "", backend.last.Database)
	agent := WithPrincipal(context.Background(), Principal{Actor: "agent:a", Role: "viewer",
		Kind: KindAgent, Scopes: []Scope{ScopeRead}, Databases: []string{"tenant_a"}})
	response = invoke(t, NewServer(backend), agent, toolCall(toolRestoreDrills, `{}`))
	require.Equal(t, 0, response.Error.Code, "an agent with read may ask")
	require.Equal(t, "tenant_a", strings.Join(backend.last.Databases, ","))
}

func TestDrillToolsRefuseBadArguments(t *testing.T) {
	backend := &drillRecordingBackend{}
	for _, args := range []string{`{"limit":5}`, `{"database":"../x"}`, `{"database":7}`,
		`{"database":"orders","extra":true}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolRestoreDrills, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls, "refusals never reach the backend")
}

func TestDrillToolsErrors(t *testing.T) {
	backend := &drillRecordingBackend{err: ErrRestoreDrillsDatabaseNotFound}
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall(toolRestoreDrills, `{"database":"other"}`))
	require.Equal(t, codeNotFound, response.Error.Code, "unknown or hidden database")
	backend.err = errors.New("read failed")
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall(toolRestoreDrills, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolRestoreDrills, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).RestoreDrills(context.Background(),
		RestoreDrillsRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "no dependency")
}
