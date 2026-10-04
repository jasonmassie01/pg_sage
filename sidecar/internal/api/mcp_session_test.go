package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// MCP over HTTP is token-only: a dashboard session cookie never
// authenticates /api/v1/mcp (a browser sends it cross-site; MCP clients
// send bearer tokens). A token request is the token's identity alone,
// whatever cookie comes along.

func sessionMCPFixture(t *testing.T) (*surfaceFixture, *mcpAuthzBackend) {
	t.Helper()
	pool := surfacePool(t)
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	backend := &mcpAuthzBackend{}
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(backend), nil, nil)
	require.NoError(t, err)
	return surfaceRouter(t, pool, cfg, &RuntimeDeps{MCPHandler: runtime.HTTPHandler()}),
		backend
}

func (f *surfaceFixture) requestBearer(t *testing.T, path, body, secret string) (int,
	string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

func TestMCPSessionCookieAloneIsRefused(t *testing.T) {
	f, backend := sessionMCPFixture(t)
	f.login(t, "admin")
	status, _ := f.request(t, "GET", "/api/v1/mcp/tokens", "")
	require.Equal(t, http.StatusOK, status, "the session itself is valid")
	for _, body := range []string{mcpProposeBody,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`} {
		status, raw := f.request(t, "POST", "/api/v1/mcp", body)
		require.Equal(t, http.StatusUnauthorized, status, raw)
		var refusal map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &refusal))
		require.Equal(t, "mcp_token_required", refusal["code"])
		require.Contains(t, refusal["error"], "token")
		require.Contains(t, refusal["error"], "MCP tokens")
	}
	require.Zero(t, backend.proposals)
	require.Zero(t, backend.reads)
}

func TestMCPTokenWithAnotherUsersCookieActsAsTheToken(t *testing.T) {
	f, backend := sessionMCPFixture(t)
	f.login(t, "admin") // the cookie jar now carries an admin session
	readOnly := mcpRoleToken(t, f.pool, "viewer")
	status, raw := f.requestBearer(t, "/api/v1/mcp", mcpProposeBody, readOnly.Secret)
	require.Equal(t, http.StatusOK, status, raw)
	require.Contains(t, raw, `"isError":true`)
	require.Contains(t, raw, `"scope_required"`)
	require.Zero(t, backend.proposals, "the admin cookie lent the token no authority")

	full := mcpRoleToken(t, f.pool, "operator")
	status, raw = f.requestBearer(t, "/api/v1/mcp", mcpProposeBody, full.Secret)
	require.Equal(t, http.StatusOK, status, raw)
	require.Equal(t, 1, backend.proposals)
	require.Equal(t, "mcp:token:"+full.ID, backend.actor, "recorded as the token")
}

func TestMCPInvalidTokenWithValidCookieIsRefused(t *testing.T) {
	f, backend := sessionMCPFixture(t)
	f.login(t, "admin")
	status, _ := f.requestBearer(t, "/api/v1/mcp", mcpProposeBody, "pgs_mcp_not_a_token")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Zero(t, backend.proposals)
}
