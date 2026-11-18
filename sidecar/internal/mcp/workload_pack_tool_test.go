package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// evid-02c: get_workload_pack is a read tool that returns a workload
// pack's model tier only; its description says it never returns the local
// tier. A principal restricted to other databases gets the principal error
// and the backend is never called.
// No concurrency test here: the tool holds no state; ForModel's concurrent
// derivations are covered in workpack.

const testPackSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// workloadPackCalls records get_workload_pack calls of allToolsBackend.
var workloadPackCalls struct {
	req WorkloadPackRequest
	err error
}

func (b *allToolsBackend) WorkloadPackModel(ctx context.Context,
	req WorkloadPackRequest) (any, error) {
	b.hit(ctx, "get_workload_pack")
	workloadPackCalls.req = req
	if workloadPackCalls.err != nil {
		return nil, workloadPackCalls.err
	}
	return map[string]any{"tier": "model", "content_sha256": "m", "sections": map[string]any{}},
		nil
}

func packTool(t *testing.T) Tool {
	t.Helper()
	for _, tool := range NewServer(newAllToolsBackend()).Tools() {
		if tool.Name == "get_workload_pack" {
			return tool
		}
	}
	t.Fatal("get_workload_pack is not listed")
	return Tool{}
}

func TestGetWorkloadPack_IsAReadToolThatNeverReturnsTheLocalTier(t *testing.T) {
	tool := packTool(t)
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read only")
	require.Contains(t, tool.Description, "never returns the local tier")
	require.Contains(t, string(tool.InputSchema), `"content_sha256"`)
	scope, _ := RequiredScope("get_workload_pack", nil)
	require.Equal(t, ScopeRead, scope)
}

func TestGetWorkloadPack_PassesTheBoundDatabaseAndHash(t *testing.T) {
	workloadPackCalls.err = nil
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	response := invoke(t, server, restricted("orders"), toolCall("get_workload_pack",
		`{"database":"orders","content_sha256":"`+testPackSHA+`"}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "orders", workloadPackCalls.req.Database)
	require.Equal(t, testPackSHA, workloadPackCalls.req.ContentSHA256)
	require.Equal(t, "model", structuredContent(t, response)["tier"])
}

func TestGetWorkloadPack_AnotherDatabasesPackGetsThePrincipalError(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	response := invoke(t, server, restricted("orders"), toolCall("get_workload_pack",
		`{"database":"billing","content_sha256":"`+testPackSHA+`"}`))
	require.Equal(t, -32003, response.Error.Code)
	require.Zero(t, backend.calls())
}

func TestGetWorkloadPack_InvalidArgumentsNeverReachTheBackend(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders"))
	for _, args := range []string{`{}`, `{"content_sha256":"abc"}`,
		`{"content_sha256":"` + strings.ToUpper(testPackSHA) + `"}`,
		`{"content_sha256":"` + testPackSHA + `","tier":"local"}`} {
		response := invoke(t, server, restricted("orders"), toolCall("get_workload_pack", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Zero(t, backend.calls())
}

func TestGetWorkloadPack_BackendErrorsAreDistinguishable(t *testing.T) {
	defer func() { workloadPackCalls.err = nil }()
	server := newAllToolsBackend().server().WithDirectory(fleetOf("orders"))
	call := toolCall("get_workload_pack", `{"content_sha256":"`+testPackSHA+`"}`)
	workloadPackCalls.err = ErrWorkloadPackNotFound
	response := invoke(t, server, restricted("orders"), call)
	require.Equal(t, -32004, response.Error.Code)
	workloadPackCalls.err = errors.Join(ErrWorkloadPackRefused,
		errors.New("schema.json field tables[0] holds a string literal"))
	response = invoke(t, server, restricted("orders"), call)
	require.Equal(t, codeInternal, response.Error.Code)
	require.Contains(t, response.Error.Message, "schema.json")
	workloadPackCalls.err = errors.New("pool exploded: password=hunter2")
	response = invoke(t, server, restricted("orders"), call)
	require.Equal(t, "internal error", response.Error.Message)
}
