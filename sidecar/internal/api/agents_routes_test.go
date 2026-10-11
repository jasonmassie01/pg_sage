package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// /api/v1/agents (spec §8.3) on the real router with a real store:
// role gates, the sponsor requirement, error statuses (§8.1), paging, the
// two-person rule for widening changes and agent token minting.

const agentsPath = "/api/v1/agents"

func agentRouter(t *testing.T, pool *pgxpool.Pool, user *auth.User,
	singleOperator bool) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.SingleOperatorMode = singleOperator
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(nil, cfg, pool, nil, nil, nil, &RuntimeDeps{}, inject)
}

func agentBody(name string, sponsor int, ceiling string) string {
	return fmt.Sprintf(`{"name":%q,"sponsor_user_id":%d,"profile":"readonly-analyst",`+
		`"env_ceiling":%q}`, name, sponsor, ceiling)
}

func agentName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func createAgent(t *testing.T, h http.Handler, sponsor int, ceiling string) map[string]any {
	t.Helper()
	code, body := tokenCall(t, h, http.MethodPost, agentsPath,
		agentBody(agentName("route-bot"), sponsor, ceiling))
	require.Equal(t, http.StatusCreated, code, body)
	return decodeObject(t, body)
}

func TestAgentRoutes_CreateGetList(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	created := createAgent(t, h, admin.ID, "stage")
	id, _ := created["id"].(string)
	require.True(t, agentguard.ValidID(id), "id %q", id)
	require.Equal(t, float64(admin.ID), created["sponsor_user_id"])
	require.Equal(t, "stage", created["env_ceiling"])
	require.Equal(t, "active", created["status"])
	require.Equal(t, true, created["sponsor_active"])
	require.Equal(t, admin.Email, created["created_by"])

	operator := agentRouter(t, pool, tokenRouteUser(t, pool, auth.RoleOperator), false)
	code, body := tokenCall(t, operator, http.MethodGet, agentsPath+"/"+id, "")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, created["name"], decodeObject(t, body)["name"])

	code, body = tokenCall(t, operator, http.MethodGet, agentsPath+"?limit=200", "")
	require.Equal(t, http.StatusOK, code, body)
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	found := false
	for _, it := range page.Items {
		found = found || it["id"] == id
	}
	require.True(t, found, "created agent listed")
	code, _ = tokenCall(t, operator, http.MethodGet, agentsPath+"?limit=201", "")
	require.Equal(t, http.StatusUnprocessableEntity, code)
	code, _ = tokenCall(t, operator, http.MethodGet, agentsPath+"?limit=x", "")
	require.Equal(t, http.StatusUnprocessableEntity, code)
	code, _ = tokenCall(t, operator, http.MethodGet, agentsPath+"/agp_aaaaaaaaaaaaaaaaaaaa", "")
	require.Equal(t, http.StatusNotFound, code)
}

func TestAgentRoutes_CreateRefusals(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	taken := createAgent(t, h, admin.ID, "dev")["name"].(string)
	cases := map[string]struct {
		body string
		code int
	}{
		"duplicate":       {agentBody(taken, admin.ID, "dev"), http.StatusConflict},
		"no sponsor":      {agentBody(agentName("x"), 0, "dev"), http.StatusUnprocessableEntity},
		"missing sponsor": {agentBody(agentName("x"), 2147483000, "dev"), 422},
		"bad name":        {agentBody("Bad Name", admin.ID, "dev"), 422},
		"bad ceiling":     {agentBody(agentName("x"), admin.ID, "qa"), 422},
		"unknown field":   {`{"name":"x-y","sponsor_user_id":1,"superuser":true}`, 422},
		"not json":        {`{"name":`, 422},
	}
	for name, c := range cases {
		code, body := tokenCall(t, h, http.MethodPost, agentsPath, c.body)
		require.Equal(t, c.code, code, "%s: %s", name, body)
	}
}

func TestAgentRoutes_RoleGates(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	id := createAgent(t, agentRouter(t, pool, admin, false), admin.ID, "dev")["id"].(string)
	operator := agentRouter(t, pool, tokenRouteUser(t, pool, auth.RoleOperator), false)
	viewer := agentRouter(t, pool, tokenRouteUser(t, pool, auth.RoleViewer), false)
	anon := agentRouter(t, pool, nil, false)
	for _, call := range []struct {
		h            http.Handler
		method, path string
		want         int
	}{
		{operator, http.MethodPost, agentsPath, http.StatusForbidden},
		{operator, http.MethodPatch, agentsPath + "/" + id, http.StatusForbidden},
		{operator, http.MethodPost, agentsPath + "/" + id + "/tokens", http.StatusForbidden},
		{viewer, http.MethodGet, agentsPath, http.StatusForbidden},
		{viewer, http.MethodGet, agentsPath + "/" + id, http.StatusForbidden},
		{anon, http.MethodGet, agentsPath, http.StatusUnauthorized},
	} {
		code, body := tokenCall(t, call.h, call.method, call.path, `{}`)
		require.Equal(t, call.want, code, "%s %s: %s", call.method, call.path, body)
	}
}

func TestAgentRoutes_PatchNarrowingAndTwoPersonRule(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	id := createAgent(t, h, admin.ID, "stage")["id"].(string)
	path := agentsPath + "/" + id
	other := tokenRouteUser(t, pool, auth.RoleOperator)
	code, body := tokenCall(t, h, http.MethodPatch, path,
		fmt.Sprintf(`{"env_ceiling":"dev","sponsor_user_id":%d}`, other.ID))
	require.Equal(t, http.StatusOK, code, "narrowing and a sponsor change: %s", body)
	got := decodeObject(t, body)
	require.Equal(t, "dev", got["env_ceiling"])
	require.Equal(t, float64(other.ID), got["sponsor_user_id"])
	for _, widen := range []string{`{"env_ceiling":"prod"}`, `{"profile":"coding-agent"}`} {
		code, body = tokenCall(t, h, http.MethodPatch, path, widen)
		require.Equal(t, http.StatusForbidden, code, body)
		require.Contains(t, body, "two_person_required")
	}
	p, err := agentguard.NewStore(pool).Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, agentguard.EnvDev, p.EnvCeiling, "a refused widening changes nothing")
	single := agentRouter(t, pool, admin, true)
	code, _ = tokenCall(t, single, http.MethodPatch, path, `{"env_ceiling":"prod"}`)
	require.Equal(t, http.StatusUnprocessableEntity, code, "single operator needs a reason")
	code, body = tokenCall(t, single, http.MethodPatch, path,
		`{"env_ceiling":"prod","reason":"lifeos reads prod"}`)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "prod", decodeObject(t, body)["env_ceiling"])
	code, _ = tokenCall(t, h, http.MethodPatch, path, `{"status":"frozen"}`)
	require.Equal(t, http.StatusUnprocessableEntity, code, "freeze has its own endpoint")
	code, body = tokenCall(t, h, http.MethodPatch, path, `{"status":"retired"}`)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "retired", decodeObject(t, body)["status"])
	code, _ = tokenCall(t, h, http.MethodPatch, path, `{"env_ceiling":"dev"}`)
	require.Equal(t, http.StatusConflict, code, "a retired agent is final")
}

func TestAgentRoutes_MintToken(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	id := createAgent(t, h, admin.ID, "prod")["id"].(string)
	path := agentsPath + "/" + id + "/tokens"
	body := `{"name":"ci","scopes":["read","propose"],"databases":["orders"],` +
		`"expires_in_days":30}`
	code, resp := tokenCall(t, h, http.MethodPost, path, body)
	require.Equal(t, http.StatusCreated, code, resp)
	tok := decodeObject(t, resp)
	secret, _ := tok["token"].(string)
	require.True(t, strings.HasPrefix(secret, mcptoken.SecretPrefix))
	require.Equal(t, id, tok["principal_id"])
	require.Equal(t, "agent", tok["kind"])
	grant, err := mcptoken.NewStore(pool).Validate(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, id, grant.PrincipalID)
	for name, c := range map[string]struct {
		path, body string
		code       int
	}{
		"approve":  {path, strings.Replace(body, `"propose"`, `"approve"`, 1), 422},
		"91 days":  {path, strings.Replace(body, "30", "91", 1), 422},
		"0 days":   {path, strings.Replace(body, "30", "0", 1), 422},
		"no agent": {agentsPath + "/agp_aaaaaaaaaaaaaaaaaaaa/tokens", body, 404},
	} {
		code, resp = tokenCall(t, h, http.MethodPost, c.path, c.body)
		require.Equal(t, c.code, code, "%s: %s", name, resp)
		require.NotContains(t, resp, mcptoken.SecretPrefix)
	}
	_, err = agentguard.NewStore(pool).SetStatus(context.Background(), id,
		agentguard.StatusFrozen, "anomaly")
	require.NoError(t, err)
	code, resp = tokenCall(t, h, http.MethodPost, path, body)
	require.Equal(t, http.StatusConflict, code, resp)
	require.Contains(t, resp, "agent_frozen")
}
