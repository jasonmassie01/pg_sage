package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Bearer edge cases that need no database: no control pool, and a control
// pool that fails (a storage failure must not read as a bad token).

const unitListFacts = `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
	`"params":{"name":"list_facts","arguments":{"database":"orders"}}}`

// closedPool is a pool whose every query fails ("closed pool").
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	pool.Close()
	return pool
}

func countingHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(http.StatusOK)
	})
}

func bearerRequest(authz string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, mcpEndpointPath, strings.NewReader(unitListFacts))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authz)
	return req
}

func TestMCPBearerWithoutControlPoolIsRefused(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	backend := &bearerBackend{}
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(backend), nil, nil)
	require.NoError(t, err)
	h := NewRouterFullRuntime(nil, cfg, nil, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, SessionAuthMiddleware(nil))

	secret := mcptoken.SecretPrefix + strings.Repeat("A", 43)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerRequest("Bearer "+secret))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), secret)
	require.Empty(t, backend.snapshot())
}

func TestMCPBearerStorageFailureIsUnavailable(t *testing.T) {
	calls := 0
	h := bindMCPPrincipal(countingHandler(&calls), mcptoken.NewStore(closedPool(t)))
	secret := mcptoken.SecretPrefix + strings.Repeat("B", 43)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerRequest("Bearer "+secret))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), secret)
	require.Zero(t, calls)

	// A malformed token is refused before storage is consulted: 401, not 503.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, bearerRequest("Bearer pgs_mcp_short"))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Zero(t, calls)
}

func TestMCPBearerCredentialParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer pgs_mcp_x", "pgs_mcp_x", true},
		{"bearer pgs_mcp_x", "pgs_mcp_x", true},
		{"Bearer   pgs_mcp_x  ", "pgs_mcp_x", true},
		{"Bearer ", "", true},
		{"Basic dXNlcjpwYXNz", "", false},
		{"Bearer", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		req := bearerRequest(tc.header)
		got, ok := bearerCredential(req)
		require.Equal(t, tc.ok, ok, "header %q", tc.header)
		require.Equal(t, tc.want, got, "header %q", tc.header)
		// Token-only endpoint: the session middleware leaves every MCP
		// request to the token check, whatever its header.
		require.True(t, isMCPTokenRequest(req), "header %q", tc.header)
	}
	other := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/tokens", nil)
	other.Header.Set("Authorization", "Bearer pgs_mcp_x")
	require.False(t, isMCPTokenRequest(other), "only the MCP endpoint takes tokens")
}

func TestMCPTokenRoutesStorageFailureIsInternal(t *testing.T) {
	store := mcptoken.NewStore(closedPool(t))
	inject := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), userContextKey, testAdminUser()))
	}
	w := httptest.NewRecorder()
	listMCPTokensHandler(store)(w, inject(httptest.NewRequest(http.MethodGet,
		"/api/v1/mcp/tokens", nil)))
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "closed pool", "storage details stay server-side")

	body := `{"name":"x","kind":"operator","scopes":["read"],"databases":["orders"],` +
		`"expires_in_days":7,"owner_user_id":7}`
	w = httptest.NewRecorder()
	createMCPTokenHandler(store)(w, inject(httptest.NewRequest(http.MethodPost,
		"/api/v1/mcp/tokens", strings.NewReader(body))))
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), mcptoken.SecretPrefix)
}
