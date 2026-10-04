package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G6-B02 / SURF-01: mutating MCP tools require an operator or admin
// principal. A viewer, or a request with no bound principal, is
// refused before the backend is reached.

var mutatingToolCalls = map[string]string{
	"propose_policy_change": `{"delta":{"budgets":{"storage_bytes":0}}}`,
	"request_change":        `{"intent":{"kind":"optimize_query","query_id":1}}`,
	"optimize_query":        `{"goal":"latency","query_id":1}`,
	// apply_migration's schema now matches its executor: table + sql.
	"apply_migration":        `{"table":"public.t","sql":"ALTER TABLE public.t ADD UNIQUE (c)"}`,
	"ensure_fk_indexes":      `{"schema":"public"}`,
	"declare_table_contract": `{"table":"public.events","append_only":true}`,
	"register_consumer":      `{"slot_name":"s1","owner":"agent"}`,
	"set_maintenance_policy": `{"scope":{},"patch":{"x":1}}`,
}

var readToolCalls = map[string]string{
	"get_policy":           `{}`,
	"get_ledger":           `{"filter":{}}`,
	"get_guarantee_status": `{}`,
	"get_value":            `{}`,
}

func toolCall(name, arguments string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"` + name + `","arguments":` + arguments + `}}`
}

func TestMutatingToolsRejectViewerPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(),
		Principal{Actor: "user:3", Role: "viewer"})
	for name, args := range mutatingToolCalls {
		backend := &intentRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall(name, args))
		require.Equal(t, -32001, response.Error.Code, name)
		require.Contains(t, response.Error.Message, "operator", name)
		require.Equal(t, 0, backend.totalCalls(), name)
		require.Equal(t, 0, backend.intentCalls, name)
	}
}

func TestMutatingToolsRejectMissingPrincipal(t *testing.T) {
	for name, args := range mutatingToolCalls {
		backend := &intentRecordingBackend{}
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(name, args))
		require.Equal(t, -32001, response.Error.Code, name)
		require.Equal(t, 0, backend.totalCalls(), name)
		require.Equal(t, 0, backend.intentCalls, name)
	}
}

func TestReadToolsAllowViewerPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(),
		Principal{Actor: "user:3", Role: "viewer"})
	for name, args := range readToolCalls {
		backend := &intentRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall(name, args))
		require.Empty(t, response.Error.Code, name)
		require.Equal(t, 1, backend.totalCalls()+backend.intentCalls, name)
	}
}

func TestMutatingToolsAllowOperatorAndCarryActor(t *testing.T) {
	for _, role := range []string{"operator", "admin"} {
		ctx := WithPrincipal(context.Background(),
			Principal{Actor: "user:2", Role: role})
		for name, args := range mutatingToolCalls {
			backend := &intentRecordingBackend{}
			response := invoke(t, NewServer(backend), ctx,
				toolCall(name, args))
			require.Empty(t, response.Error.Code, name)
			require.Equal(t, 1, backend.totalCalls()+backend.intentCalls, name)
			require.Equal(t, "mcp:user:2", backend.lastActor, name)
		}
	}
}

func TestToolsListIsAvailableToViewers(t *testing.T) {
	ctx := WithPrincipal(context.Background(),
		Principal{Actor: "user:3", Role: "viewer"})
	response := invoke(t, NewServer(&intentRecordingBackend{}), ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	require.Empty(t, response.Error.Code)
}

func TestActorFromContext(t *testing.T) {
	require.Equal(t, "mcp-agent", ActorFromContext(context.Background()))
	ctx := WithPrincipal(context.Background(),
		Principal{Actor: "user:9", Role: "operator"})
	require.Equal(t, "mcp:user:9", ActorFromContext(ctx))
	blank := WithPrincipal(context.Background(),
		Principal{Actor: "  ", Role: "operator"})
	require.Equal(t, "mcp-agent", ActorFromContext(blank))
}

// intentRecordingBackend implements both Backend and IntentBackend and
// records the actor visible to the backend.
type intentRecordingBackend struct {
	recordingBackend
	intentCalls int
	lastActor   string
}

func (b *intentRecordingBackend) ProposePolicyChange(
	ctx context.Context, request PolicyProposalRequest,
) (PolicyProposalResult, error) {
	b.lastActor = ActorFromContext(ctx)
	return b.recordingBackend.ProposePolicyChange(ctx, request)
}

func (b *intentRecordingBackend) RequestChange(
	ctx context.Context, request ChangeRequest,
) (ChangeResult, error) {
	b.lastActor = ActorFromContext(ctx)
	return b.recordingBackend.RequestChange(ctx, request)
}

func (b *intentRecordingBackend) RequestIntent(
	ctx context.Context, _ string, _ json.RawMessage,
) (any, error) {
	b.intentCalls++
	b.lastActor = ActorFromContext(ctx)
	return map[string]any{"ok": true}, nil
}
