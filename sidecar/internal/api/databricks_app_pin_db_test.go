package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// databricksAppRouterAs is the Apps-mode router whose resolver always
// returns user, over a lazy pool that is never reached by these routes.
func databricksAppRouterAs(t *testing.T, user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	mw := databricksAppAuthMiddleware(func(context.Context, string, string) (*auth.User, error) {
		return user, nil
	})
	return newTestRouter(fleet.NewManager(cfg), cfg, unreachablePool(t), nil, nil, nil,
		&RuntimeDeps{DatabricksAppAuth: true}, mw)
}

func appCall(h http.Handler, method, path, email, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Content-Type", "application/json") // the router refuses other bodies
	r.Header.Set("X-Forwarded-Email", email)
	if user != "" {
		r.Header.Set("X-Forwarded-User", user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func appPin(t *testing.T, pool *pgxpool.Pool, id int) *string {
	t.Helper()
	var pin *string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT databricks_user_pin FROM sage.users WHERE id = $1`, id).Scan(&pin))
	return pin
}

// After a mismatch in enforce, the bootstrapped admin gets 403 on an admin
// route. A second admin resets the pin; the owner then gets 200 and the
// new value is pinned.
func TestDatabricksAppPinEnforceLocksOwnerUntilReset(t *testing.T) {
	pool := surfacePool(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	app, h := enforcedAppRouter(t, pool, stamp)

	w := appCall(h, http.MethodGet, "/api/v1/auth/me", app.AdminEmail, "owner-1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	owner := decodeObject(t, w.Body.String())
	ownerID := int(owner["id"].(float64))
	require.Equal(t, auth.RoleAdmin, owner["role"])
	require.Equal(t, auth.DatabricksAppUserPin(app.Workspace, "owner-1"), *appPin(t, pool, ownerID))

	w = appCall(h, http.MethodGet, "/api/v1/users", app.AdminEmail, "owner-2")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.True(t, strings.Contains(w.Body.String(), "databricks_app_identity_changed"),
		"body %s", w.Body.String())

	second := fmt.Sprintf("pin-second-%d@example.com", stamp)
	w = appCall(h, http.MethodGet, "/api/v1/auth/me", second, "second-1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	secondID := int(decodeObject(t, w.Body.String())["id"].(float64))
	resetPath := fmt.Sprintf("/api/v1/users/%d/identity-pin/reset", ownerID)
	w = appCall(h, http.MethodPost, resetPath, second, "second-1")
	require.Equal(t, http.StatusForbidden, w.Code, "a viewer reset the pin: %s", w.Body.String())
	_, err := pool.Exec(ctx, `UPDATE sage.users SET role = 'admin' WHERE id = $1`, secondID)
	require.NoError(t, err)

	var since int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(max(id), 0) FROM sage.auth_audit`).Scan(&since))
	w = appCall(h, http.MethodPost, resetPath, second, "second-1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, true, decodeObject(t, w.Body.String())["previously_pinned"])
	ev := eventsBy(t, pool, secondID, since)
	require.Len(t, ev, 1, "reset events %+v", ev)
	require.Equal(t, auth.AuditDatabricksAppPinReset, ev[0].Event)
	require.Equal(t, ownerID, ev[0].Target)
	require.False(t, strings.Contains(ev[0].Raw, "@"), "reset detail leaks email: %s", ev[0].Raw)

	w = appCall(h, http.MethodGet, "/api/v1/users", app.AdminEmail, "owner-2")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, auth.DatabricksAppUserPin(app.Workspace, "owner-2"), *appPin(t, pool, ownerID))

	w = appCall(h, http.MethodPost, "/api/v1/users/2147483000/identity-pin/reset",
		second, "second-1")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// enforcedAppRouter bootstraps a fresh owner in an enforce-mode workspace
// and returns the Apps-mode router over the real pool.
func enforcedAppRouter(
	t *testing.T, pool *pgxpool.Pool, stamp int64,
) (auth.DatabricksAppConfig, http.Handler) {
	t.Helper()
	app := auth.DatabricksAppConfig{
		Workspace:   fmt.Sprintf("https://pin-api-%d.cloud.databricks.com", stamp),
		AdminEmail:  fmt.Sprintf("pin-owner-%d@example.com", stamp),
		IdentityPin: auth.DatabricksAppPinEnforce,
	}
	require.NoError(t, auth.BootstrapDatabricksAppAdmin(context.Background(), pool, app))
	cfg := config.DefaultConfig()
	h := newTestRouter(fleet.NewManager(cfg), cfg, pool, nil, nil, nil,
		&RuntimeDeps{DatabricksAppAuth: true}, DatabricksAppAuthMiddleware(pool, app))
	return app, h
}
