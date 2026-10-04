package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Admin-only MCP token management routes on the real router and control
// database. The session user is injected; the routes, store and schema
// are real.

const tokenRoutesPath = "/api/v1/mcp/tokens"

func tokenRouteUser(t *testing.T, pool *pgxpool.Pool, role string) *auth.User {
	t.Helper()
	email := fmt.Sprintf("mcptok-%s-%d@example.com", role, time.Now().UnixNano())
	id, err := auth.CreateUser(context.Background(), pool, email, "password-123", role)
	require.NoError(t, err)
	return &auth.User{ID: id, Email: email, Role: role}
}

func tokenRouter(t *testing.T, pool *pgxpool.Pool, user *auth.User) http.Handler {
	t.Helper()
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(&mcpAuthzBackend{}), nil, nil)
	require.NoError(t, err)
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(nil, cfg, pool, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, inject)
}

func tokenCall(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	return out
}

func agentTokenBody(name string) string {
	return fmt.Sprintf(`{"name":%q,"kind":"agent","scopes":["read","propose"],`+
		`"databases":["orders"],"expires_in_days":30}`, name)
}

func countTokensNamed(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.mcp_tokens WHERE name = $1`, name).Scan(&n))
	return n
}

func listedToken(t *testing.T, h http.Handler, id string) (map[string]any, string) {
	t.Helper()
	code, body := tokenCall(t, h, http.MethodGet, tokenRoutesPath, "")
	require.Equal(t, http.StatusOK, code, body)
	var out struct {
		Tokens []map[string]any `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	require.NotNil(t, out.Tokens, "tokens must be a JSON array")
	for _, tok := range out.Tokens {
		if tok["id"] == id {
			return tok, body
		}
	}
	t.Fatalf("token %s not listed in %s", id, body)
	return nil, body
}

func TestMCPTokenRoutesAdminCreateListRevoke(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := tokenRouter(t, pool, admin)
	name := fmt.Sprintf("route-agent-%d", time.Now().UnixNano())

	code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath, agentTokenBody(name))
	require.Equal(t, http.StatusCreated, code, body)
	created := decodeObject(t, body)
	secret, _ := created["token"].(string)
	id, _ := created["id"].(string)
	require.True(t, strings.HasPrefix(secret, mcptoken.SecretPrefix), body)
	require.NotEmpty(t, id)
	require.Equal(t, "agent", created["kind"])
	require.Equal(t, []any{"read", "propose"}, created["scopes"])
	require.Equal(t, []any{"orders"}, created["databases"])
	require.Equal(t, admin.Email, created["created_by"])
	require.Equal(t, secret[:12], created["prefix"])

	entry, listBody := listedToken(t, h, id)
	_, hasSecret := entry["token"]
	require.False(t, hasSecret, "list must never carry the secret")
	require.NotContains(t, listBody, secret)
	require.NotContains(t, listBody, mcptoken.HashSecret(secret))
	require.NotContains(t, listBody, "token_hash")
	require.Equal(t, name, entry["name"])

	grant, err := mcptoken.NewStore(pool).Validate(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, id, grant.TokenID)

	code, body = tokenCall(t, h, http.MethodDelete, tokenRoutesPath+"/"+id, "")
	require.Equal(t, http.StatusOK, code, body)
	revoked := decodeObject(t, body)
	require.NotEmpty(t, revoked["revoked_at"])
	require.Equal(t, admin.Email, revoked["revoked_by"])
	_, err = mcptoken.NewStore(pool).Validate(context.Background(), secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)

	entry, _ = listedToken(t, h, id)
	require.NotEmpty(t, entry["revoked_at"])
	code, body = tokenCall(t, h, http.MethodDelete, tokenRoutesPath+"/"+id, "")
	require.Equal(t, http.StatusOK, code, "revoke is idempotent: %s", body)
}

func TestMCPTokenRoutesForbiddenForNonAdmins(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	code, body := tokenCall(t, tokenRouter(t, pool, admin), http.MethodPost,
		tokenRoutesPath, agentTokenBody(fmt.Sprintf("victim-%d", time.Now().UnixNano())))
	require.Equal(t, http.StatusCreated, code, body)
	victim, _ := decodeObject(t, body)["id"].(string)

	for _, role := range []string{auth.RoleOperator, auth.RoleViewer} {
		h := tokenRouter(t, pool, tokenRouteUser(t, pool, role))
		name := fmt.Sprintf("forbidden-%s-%d", role, time.Now().UnixNano())
		code, body = tokenCall(t, h, http.MethodGet, tokenRoutesPath, "")
		require.Equal(t, http.StatusForbidden, code, "%s list: %s", role, body)
		require.NotContains(t, body, victim)
		code, _ = tokenCall(t, h, http.MethodPost, tokenRoutesPath, agentTokenBody(name))
		require.Equal(t, http.StatusForbidden, code, "%s create", role)
		require.Equal(t, 0, countTokensNamed(t, pool, name))
		code, _ = tokenCall(t, h, http.MethodDelete, tokenRoutesPath+"/"+victim, "")
		require.Equal(t, http.StatusForbidden, code, "%s revoke", role)
	}
	entry, _ := listedToken(t, tokenRouter(t, pool, admin), victim)
	require.Nil(t, entry["revoked_at"], "a forbidden revoke must not revoke")

	anon := tokenRouter(t, pool, nil)
	code, _ = tokenCall(t, anon, http.MethodGet, tokenRoutesPath, "")
	require.Equal(t, http.StatusUnauthorized, code)
}

func TestMCPTokenRoutesRejectAgentApprove(t *testing.T) {
	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	name := fmt.Sprintf("agent-approve-%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"name":%q,"kind":"agent","scopes":["read","approve"],`+
		`"databases":["*"],"expires_in_days":7}`, name)

	code, resp := tokenCall(t, h, http.MethodPost, tokenRoutesPath, body)
	require.Equal(t, http.StatusBadRequest, code, resp)
	require.Contains(t, strings.ToLower(resp), "operator token")
	require.NotContains(t, resp, mcptoken.SecretPrefix)
	require.Equal(t, 0, countTokensNamed(t, pool, name))
}

func operatorTokenBody(name string, owner int) string {
	ownerField := ""
	if owner != 0 {
		ownerField = fmt.Sprintf(`,"owner_user_id":%d`, owner)
	}
	return fmt.Sprintf(`{"name":%q,"kind":"operator","scopes":["read","propose","approve"],`+
		`"databases":["*"],"expires_in_days":7%s}`, name, ownerField)
}

func TestMCPTokenRoutesOperatorTokenOwner(t *testing.T) {
	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	viewer := tokenRouteUser(t, pool, auth.RoleViewer)
	operator := tokenRouteUser(t, pool, auth.RoleOperator)
	stamp := time.Now().UnixNano()

	for owner, label := range map[int]string{0: "no owner", viewer.ID: "viewer owner"} {
		name := fmt.Sprintf("op-%d-%d", owner, stamp)
		code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
			operatorTokenBody(name, owner))
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", label, body)
		require.Equal(t, 0, countTokensNamed(t, pool, name))
	}

	code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
		operatorTokenBody(fmt.Sprintf("op-ok-%d", stamp), operator.ID))
	require.Equal(t, http.StatusCreated, code, body)
	created := decodeObject(t, body)
	require.Equal(t, "operator", created["kind"])
	require.Equal(t, float64(operator.ID), created["owner_user_id"])
	require.Equal(t, []any{"read", "propose", "approve"}, created["scopes"])
	require.Equal(t, []any{"*"}, created["databases"])
}

func lifetimeBody(name string, days int) string {
	return fmt.Sprintf(`{"name":%q,"kind":"agent","scopes":["read"],`+
		`"databases":["orders"],"expires_in_days":%d}`, name, days)
}

func TestMCPTokenRoutesLifetimeBounds(t *testing.T) {
	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	stamp := time.Now().UnixNano()
	for _, days := range []int{0, 91, -1, 3650} {
		name := fmt.Sprintf("life-bad-%d-%d", days, stamp)
		code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath, lifetimeBody(name, days))
		require.Equal(t, http.StatusBadRequest, code, "%d days: %s", days, body)
		require.Equal(t, 0, countTokensNamed(t, pool, name))
	}
	for _, days := range []int{1, 90} {
		name := fmt.Sprintf("life-ok-%d-%d", days, stamp)
		code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath, lifetimeBody(name, days))
		require.Equal(t, http.StatusCreated, code, "%d days: %s", days, body)
		created := decodeObject(t, body)
		createdAt, err := time.Parse(time.RFC3339Nano, created["created_at"].(string))
		require.NoError(t, err)
		expiresAt, err := time.Parse(time.RFC3339Nano, created["expires_at"].(string))
		require.NoError(t, err)
		want := createdAt.Add(time.Duration(days) * 24 * time.Hour)
		require.WithinDuration(t, want, expiresAt, time.Minute)
	}
}

func TestMCPTokenRoutesRejectMalformedRequests(t *testing.T) {
	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	bad := map[string]string{
		"truncated json": `{"name":`,
		"array body":     `[]`,
		"empty body":     ``,
		"name not str":   `{"name":5,"kind":"agent","scopes":["read"],"databases":["a"]}`,
		"empty name": `{"name":"","kind":"agent","scopes":["read"],` +
			`"databases":["orders"],"expires_in_days":7}`,
		"unknown scope": `{"name":"x","kind":"agent","scopes":["read","bogus"],` +
			`"databases":["orders"],"expires_in_days":7}`,
		"no databases": `{"name":"x","kind":"agent","scopes":["read"],` +
			`"databases":[],"expires_in_days":7}`,
		"unknown kind": `{"name":"x","kind":"robot","scopes":["read"],` +
			`"databases":["orders"],"expires_in_days":7}`,
		"agent owner": `{"name":"x","kind":"agent","scopes":["read"],` +
			`"databases":["orders"],"expires_in_days":7,"owner_user_id":1}`,
	}
	for label, body := range bad {
		code, resp := tokenCall(t, h, http.MethodPost, tokenRoutesPath, body)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", label, resp)
		require.NotContains(t, resp, mcptoken.SecretPrefix, label)
		require.Contains(t, resp, `"error"`, label)
	}
}

func TestMCPTokenRoutesDeleteUnknownIsNotFound(t *testing.T) {
	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	for _, id := range []string{"does-not-exist", "00000000-0000-0000-0000-000000000000"} {
		code, body := tokenCall(t, h, http.MethodDelete, tokenRoutesPath+"/"+id, "")
		require.Equal(t, http.StatusNotFound, code, "%s: %s", id, body)
	}
}
