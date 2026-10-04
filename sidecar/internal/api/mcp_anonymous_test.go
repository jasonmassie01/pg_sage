package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// MCP v2: with the default configuration MCP is served over HTTP, and an
// MCP request always needs a credential. No method, no transport detail
// and no missing middleware makes it anonymous.

var anonymousMCPBodies = []string{
	`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
	`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
	`{"jsonrpc":"2.0","id":4,"method":"server/discover","params":{"_meta":` +
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
	`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"list_facts",` +
		`"arguments":{"database":"orders"}}}`,
	`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
}

func defaultMCPRouter(t *testing.T, withSession bool) (http.Handler, *bearerBackend,
	*bearerFixture) {
	t.Helper()
	pool := surfacePool(t)
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig() // no MCP settings: the defaults decide
	require.Equal(t, "http", cfg.MCP.Transport)
	backend := &bearerBackend{}
	runtime, err := mcp.NewRuntime(cfg.MCP,
		mcp.NewServer(backend).WithDirectory(bearerDirectory{}), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, runtime.HTTPHandler(), "the default transport is HTTP")
	var middlewares []func(http.Handler) http.Handler
	if withSession {
		middlewares = append(middlewares, SessionAuthMiddleware(pool))
	}
	h := NewRouterFullRuntime(nil, cfg, pool, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, middlewares...)
	return h, backend, &bearerFixture{pool: pool, store: mcptoken.NewStore(pool),
		backend: backend, h: h}
}

func anonymousPost(h http.Handler, body string, header map[string]string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestDefaultMCPOverHTTPIsNeverAnonymous(t *testing.T) {
	for _, withSession := range []bool{true, false} {
		h, backend, _ := defaultMCPRouter(t, withSession)
		for _, body := range anonymousMCPBodies {
			for _, header := range []map[string]string{nil,
				{"Authorization": "Bearer "}, {"Authorization": "Bearer pgs_mcp_garbage"},
				{"Authorization": "Basic YWRtaW46YWRtaW4="}, {"Authorization": "pgs_mcp_x"},
				{"Cookie": "sage_session=not-a-session"}} {
				code := anonymousPost(h, body, header)
				require.Equal(t, http.StatusUnauthorized, code,
					"session middleware %v, %v: %s", withSession, header, body)
			}
		}
		require.Empty(t, backend.snapshot(), "no anonymous request reached a backend")
	}
}

func TestDefaultMCPOverHTTPServesAToken(t *testing.T) {
	h, backend, f := defaultMCPRouter(t, true)
	tok := f.agentToken(t)
	code := anonymousPost(h, anonymousMCPBodies[4],
		map[string]string{"Authorization": "Bearer " + tok.Secret})
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, backend.snapshot(), "a token reaches the backend")
}
