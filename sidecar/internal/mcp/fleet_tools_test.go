package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Fleet learning: fleet_findings is the one fleet-wide read tool. It names
// no single database; a token scoped to some databases only sees those.

type fleetRecordingBackend struct {
	recordingBackend
	calls int
	last  FleetFindingsRequest
	err   error
}

func (b *fleetRecordingBackend) FleetFindings(_ context.Context,
	req FleetFindingsRequest) (any, error) {
	b.calls++
	b.last = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"findings": []any{}, "databases_scanned": 2}, nil
}

func TestFleetFindingsToolIsListedReadOnlyWithoutDatabase(t *testing.T) {
	var tool *Tool
	for _, candidate := range NewServer(&fleetRecordingBackend{}).Tools() {
		if candidate.Name == "fleet_findings" {
			c := candidate
			tool = &c
		}
	}
	require.True(t, tool != nil, "fleet_findings listed")
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
	var schema map[string]any
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	props, _ := schema["properties"].(map[string]any)
	_, hasDatabase := props["database"]
	require.True(t, !hasDatabase, "a fleet-wide tool takes no database argument")
	_, hasMin := props["min_databases"]
	require.True(t, hasMin, "min_databases argument")
}

func TestFleetFindingsToolPassesArgumentsAndViewerMayCall(t *testing.T) {
	backend := &fleetRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3",
		Role: "viewer"})
	response := invoke(t, NewServer(backend), viewer,
		toolCall("fleet_findings", `{"min_databases":5}`))
	require.Equal(t, 0, response.Error.Code, "viewer may read fleet findings")
	require.Equal(t, 1, backend.calls)
	require.Equal(t, 5, backend.last.MinDatabases)
	require.True(t, backend.last.Databases == nil, "unrestricted principal: all databases")
}

func TestFleetFindingsToolRestrictsScopedTokens(t *testing.T) {
	backend := &fleetRecordingBackend{}
	scoped := WithPrincipal(context.Background(), Principal{Actor: "token:1",
		Role: "viewer", Databases: []string{"tenant_a", "tenant_b"}})
	response := invoke(t, NewServer(backend), scoped,
		toolCall("fleet_findings", `{}`))
	require.Equal(t, 0, response.Error.Code, "scoped token may call")
	require.Equal(t, "tenant_a,tenant_b", strings.Join(backend.last.Databases, ","))
	require.Equal(t, 0, backend.last.MinDatabases)
}

func TestFleetFindingsToolRejectsBadArguments(t *testing.T) {
	backend := &fleetRecordingBackend{}
	for _, args := range []string{`{"min_databases":"x"}`, `{"database":"a"}`,
		`{"min_databases":1}`, `{"min_databases":-2}`, `{"other":1}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall("fleet_findings", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
}

func TestFleetFindingsToolBackendErrorAndMissingBackend(t *testing.T) {
	backend := &fleetRecordingBackend{err: errors.New("fan-out failed")}
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall("fleet_findings", `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall("fleet_findings", `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	require.True(t, strings.Contains(response.Error.Message, "fleet"),
		response.Error.Message)
}
