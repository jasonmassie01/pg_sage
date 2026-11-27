package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// upgrade_readiness (market-05): a read-only, per-database tool.

type upgradeRecordingBackend struct {
	recordingBackend
	calls    int
	last     UpgradeReadinessRequest
	database string
	err      error
}

func (b *upgradeRecordingBackend) UpgradeReadiness(ctx context.Context,
	req UpgradeReadinessRequest) (any, error) {
	b.calls++
	b.last = req
	b.database, _ = DatabaseFromContext(ctx)
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"checks": []any{}}, nil
}

func TestUpgradeReadinessToolIsReadOnlyPerDatabase(t *testing.T) {
	for _, tool := range NewServer(&upgradeRecordingBackend{}).Tools() {
		if tool.Name != "upgrade_readiness" {
			continue
		}
		require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
		require.True(t, strings.Contains(string(tool.InputSchema), `"database"`),
			"takes the database argument")
		return
	}
	t.Fatal("upgrade_readiness is not listed")
}

func TestUpgradeReadinessToolPassesArgumentsAndViewerMayCall(t *testing.T) {
	backend := &upgradeRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3",
		Role: "viewer"})
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, viewer, toolCall("upgrade_readiness",
		`{"target_major":18,"method":"blue_green","target_database":"pg18"}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, 18, backend.last.TargetMajor)
	require.Equal(t, "blue_green", backend.last.Method)
	require.Equal(t, "pg18", backend.last.TargetDatabase)
	require.Equal(t, "orders", backend.database)
}

func TestUpgradeReadinessToolRejectsBadArguments(t *testing.T) {
	backend := &upgradeRecordingBackend{}
	for _, args := range []string{`{}`, `{"target_major":19}`, `{"target_major":0}`,
		`{"target_major":"18"}`, `{"target_major":18,"method":"dump"}`,
		`{"target_major":18,"target_database":"a;b"}`, `{"target_major":18,"x":1}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall("upgrade_readiness", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
}

func TestUpgradeReadinessToolErrors(t *testing.T) {
	invalid := &upgradeRecordingBackend{err: fmt.Errorf("%w: target_major 17 must be "+
		"above the server's major 17", upgradecheck.ErrInvalidTarget)}
	response := invoke(t, NewServer(invalid), context.Background(),
		toolCall("upgrade_readiness", `{"target_major":17}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, "at or below the server")
	require.True(t, strings.Contains(response.Error.Message, "must be above"),
		response.Error.Message)
	failing := &upgradeRecordingBackend{err: errors.New("connection refused")}
	response = invoke(t, NewServer(failing), context.Background(),
		toolCall("upgrade_readiness", `{"target_major":18}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	require.True(t, strings.Contains(response.Error.Message, "connection refused"),
		response.Error.Message)
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall("upgrade_readiness", `{"target_major":18}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).UpgradeReadiness(context.Background(),
		UpgradeReadinessRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "unwired")
}
