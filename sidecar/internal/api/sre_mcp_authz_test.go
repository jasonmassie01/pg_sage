package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// CHECK-39 (read tools): on the real mounted router, a signed-in viewer
// reads investigations through MCP, a request without a session is
// refused before any tool runs, and the read tools never mutate.

type sreMCPBackend struct {
	mcpAuthzBackend
	investigationReads int
}

func (b *sreMCPBackend) ListInvestigations(context.Context,
	mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"items": []any{}}, nil
}

func (b *sreMCPBackend) GetInvestigation(_ context.Context,
	r mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"id": r.InvestigationID}, nil
}

func (b *sreMCPBackend) GetEvidence(_ context.Context,
	r mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"id": r.EvidenceID}, nil
}

const sreGetBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
	`{"name":"sre_get_investigation","arguments":{"investigation_id":"abc"}}}`

func TestMountedMCPViewerReadsInvestigations(t *testing.T) {
	backend := &sreMCPBackend{}
	h := mcpRouterForBackend(t, backend, testViewerUser())
	out := postMCPCall(t, h, sreGetBody)
	require.Nil(t, out["error"])
	require.Equal(t, 1, backend.investigationReads)
	require.Zero(t, backend.proposals)
}

func TestMountedMCPInvestigationToolsNeedASession(t *testing.T) {
	backend := &sreMCPBackend{}
	h := mcpRouterForBackend(t, backend, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(sreGetBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, backend.investigationReads)
}

// mcpRouterForBackend mounts the real MCP runtime and server over any
// backend on the real router, with user bound as the session.
func mcpRouterForBackend(t *testing.T, backend mcp.Backend, user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(backend), nil, nil)
	require.NoError(t, err)
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(nil, cfg, nil, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, inject)
}
