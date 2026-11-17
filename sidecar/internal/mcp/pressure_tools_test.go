package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/pressure"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// check_pressure (guard-01a): a read-only, per-database tool a backfill
// calls between batches. The check itself is tested in internal/pressure
// and through the route in internal/api. No concurrent access tests here:
// the tool keeps no state (the checker's cache is tested in its package).

type pressureRecordingBackend struct {
	recordingBackend
	calls int
	got   PressureRequest
	err   error
}

func (b *pressureRecordingBackend) CheckPressure(_ context.Context,
	req PressureRequest) (any, error) {
	b.calls++
	b.got = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"verdict": "proceed"}, nil
}

func TestPressureToolIsReadOnlyAndSaysWhenToCall(t *testing.T) {
	for _, tool := range NewServer(&pressureRecordingBackend{}).Tools() {
		if tool.Name != toolCheckPressure {
			continue
		}
		require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
		schema := string(tool.InputSchema)
		for _, arg := range []string{`"database"`, `"table"`, `"job"`, `"batch"`} {
			require.True(t, strings.Contains(schema, arg), "schema lacks "+arg+": "+schema)
		}
		require.True(t, strings.Contains(tool.Description, "between batches"), tool.Description)
		require.True(t, strings.Contains(tool.Description, "never acts"), tool.Description)
		return
	}
	t.Fatal("check_pressure is not listed")
}

func TestPressureToolBindsTheDatabaseAndPassesTheArguments(t *testing.T) {
	backend := &pressureRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, viewer, toolCall(toolCheckPressure,
		`{"table":"public.events","job":"backfill-1","batch":500}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, PressureRequest{Database: "orders", Table: "public.events",
		Job: "backfill-1", Batch: 500}, backend.got)
}

func TestPressureToolRejectsBadArguments(t *testing.T) {
	backend := &pressureRecordingBackend{}
	for _, args := range []string{`{"x":1}`, `{"table":5}`, `{"batch":"ten"}`,
		`{"batch":-1}`, `{"batch":1.5}`, `[]`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolCheckPressure, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
}

func TestPressureToolErrors(t *testing.T) {
	cases := []struct {
		err  error
		code int
		text string
	}{
		{fmt.Errorf("table x: %w", pressure.ErrUnknownTable), codeNotFound, "table"},
		{ErrPressureDatabaseNotFound, codeNotFound, "database"},
		{fmt.Errorf("%w: bad name", pressure.ErrInvalidRequest), codeInvalidParams, "bad name"},
		{context.Canceled, codeCancelled, "cancelled"},
		{errors.New("read pressure inputs of orders: refused"), codeInternal, "refused"},
	}
	for _, c := range cases {
		response := invoke(t, NewServer(&pressureRecordingBackend{err: c.err}),
			context.Background(), toolCall(toolCheckPressure, `{}`))
		require.Equal(t, c.code, response.Error.Code, c.err.Error())
		require.True(t, strings.Contains(response.Error.Message, c.text),
			response.Error.Message)
	}
	response := invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolCheckPressure, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).CheckPressure(context.Background(), PressureRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "unwired")
}
