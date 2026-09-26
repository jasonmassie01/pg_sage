package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// G6-B02 / SURF-01: exercised on the real mounted router with the
// real MCP runtime and server; only the backend is a recorder.

type mcpAuthzBackend struct {
	proposals int
	reads     int
	actor     string
}

func (b *mcpAuthzBackend) GetPolicy(
	context.Context, mcp.PolicyRequest,
) (mcp.PolicyResult, error) {
	b.reads++
	return mcp.PolicyResult{Version: 1, Profile: "staffed"}, nil
}

func (b *mcpAuthzBackend) ProposePolicyChange(
	ctx context.Context, _ mcp.PolicyProposalRequest,
) (mcp.PolicyProposalResult, error) {
	b.proposals++
	b.actor = mcp.ActorFromContext(ctx)
	return mcp.PolicyProposalResult{ProposalID: 5}, nil
}

func (b *mcpAuthzBackend) RequestChange(
	context.Context, mcp.ChangeRequest,
) (mcp.ChangeResult, error) {
	b.proposals++
	return mcp.ChangeResult{Decision: "parked"}, nil
}

func (b *mcpAuthzBackend) GetLedger(
	context.Context, mcp.LedgerRequest,
) (mcp.LedgerResult, error) {
	b.reads++
	return mcp.LedgerResult{}, nil
}

func mcpRouterForUser(
	t *testing.T, backend *mcpAuthzBackend, user *auth.User,
) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(backend), nil, nil)
	require.NoError(t, err)
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(
					r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(nil, cfg, nil, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, inject)
}

func postMCPCall(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

const mcpProposeBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
	`"params":{"name":"propose_policy_change",` +
	`"arguments":{"delta":{"budgets":{"storage_bytes":0}}}}}`

func TestMountedMCPViewerCannotMutate(t *testing.T) {
	backend := &mcpAuthzBackend{}
	h := mcpRouterForUser(t, backend, testViewerUser())

	out := postMCPCall(t, h, mcpProposeBody)
	rpcErr, ok := out["error"].(map[string]any)
	require.True(t, ok, "viewer mutation must return a JSON-RPC error")
	require.Equal(t, float64(-32001), rpcErr["code"])
	require.Zero(t, backend.proposals)
}

func TestMountedMCPViewerCanRead(t *testing.T) {
	backend := &mcpAuthzBackend{}
	h := mcpRouterForUser(t, backend, testViewerUser())

	out := postMCPCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
		`"params":{"name":"get_policy","arguments":{}}}`)
	require.Nil(t, out["error"])
	require.Equal(t, 1, backend.reads)
}

func TestMountedMCPOperatorMutatesWithBoundActor(t *testing.T) {
	backend := &mcpAuthzBackend{}
	h := mcpRouterForUser(t, backend, testOperatorUser())

	out := postMCPCall(t, h, mcpProposeBody)
	require.Nil(t, out["error"])
	require.Equal(t, 1, backend.proposals)
	require.Equal(t, "mcp:user:2", backend.actor)
}

func TestMountedMCPWithoutUserIsRejected(t *testing.T) {
	backend := &mcpAuthzBackend{}
	h := mcpRouterForUser(t, backend, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp",
		strings.NewReader(mcpProposeBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, backend.proposals)
}
