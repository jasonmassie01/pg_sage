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
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestDatabricksAppAuthRequiresUnambiguousPlatformEmail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"missing", nil}, {"empty", []string{""}},
		{"duplicate", []string{"viewer@example.com", "owner@example.com"}},
		{"list", []string{"viewer@example.com,owner@example.com"}},
		{"display name", []string{"Owner <owner@example.com>"}},
		{"whitespace", []string{" owner@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := databricksAppAuthMiddleware(func(context.Context, string) (*auth.User, error) {
				called = true
				return &auth.User{ID: 1, Role: auth.RoleAdmin}, nil
			})(http.HandlerFunc(meHandler()))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			r.Header["X-Forwarded-Email"] = tc.values
			r.Header.Set("X-Forwarded-User", "owner@example.com")
			r.Header.Set("X-Forwarded-Role", "admin")
			r.Header.Set("Authorization", "Bearer existing-agent-token")
			r.AddCookie(&http.Cookie{Name: "sage_session", Value: "existing-session"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || called {
				t.Fatalf("identity bypass status=%d resolver=%v", w.Code, called)
			}
		})
	}
}

func TestDatabricksAppAuthUsesPersistedRoleAndNoLocalSession(t *testing.T) {
	resolver := func(_ context.Context, email string) (*auth.User, error) {
		if email != "viewer@example.com" {
			t.Fatalf("wrong identity: %q", email)
		}
		return &auth.User{ID: 8, Email: email, Role: auth.RoleViewer}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("X-Forwarded-Email", "Viewer@example.com")
	r.Header.Set("X-Forwarded-Role", "admin")
	h := databricksAppAuthMiddleware(resolver)(http.HandlerFunc(meHandler()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response["auth_mode"] != auth.DatabricksAppMode || response["id"] != float64(8) || response["role"] != auth.RoleViewer || len(w.Result().Cookies()) != 0 {
		t.Fatalf("platform identity response = %d %s", w.Code, w.Body.String())
	}
	h = databricksAppAuthMiddleware(resolver)(RequireRole(auth.RoleAdmin)(http.HandlerFunc(meHandler())))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("forwarded role escalated privileges: %d", w.Code)
	}
	// The same untrusted headers must not sign in to a standalone server.
	w = httptest.NewRecorder()
	SessionAuthMiddleware(nil)(http.HandlerFunc(meHandler())).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("standalone accepted proxy identity: %d", w.Code)
	}
}

func TestDatabricksAppRouterDisablesAlternateLoginsAndLocalUsers(t *testing.T) {
	cfg := config.DefaultConfig()
	resolver := databricksAppAuthMiddleware(func(context.Context, string) (*auth.User, error) {
		return &auth.User{ID: 1, Email: "owner@example.com", Role: auth.RoleAdmin}, nil
	})
	h := NewRouterFullRuntime(fleet.NewManager(cfg), cfg, unreachablePool(t), nil, nil, nil,
		&RuntimeDeps{DatabricksAppAuth: true}, resolver)
	for _, path := range []string{
		"/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/break-glass",
		"/api/v1/auth/oauth/authorize", "/api/v1/auth/oauth/callback", "/api/v1/auth/oauth/link-grant",
		"/api/v1/users", "/api/v1/users/1/oidc-link-grant",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"owner@example.com","password":"old-password"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-Email", "owner@example.com")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("local authentication/creation route remains enabled: %d %s", w.Code, w.Body.String())
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/mode", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), auth.DatabricksAppMode) {
		t.Fatalf("public mode discovery = %d %s", w.Code, w.Body.String())
	}
}

func TestDatabricksAppRouterRejectsCrossOriginMutationAndLocalCORS(t *testing.T) {
	cfg := config.DefaultConfig()
	h := NewRouterFullRuntime(fleet.NewManager(cfg), cfg, nil, nil, nil, nil,
		&RuntimeDeps{DatabricksAppAuth: true})
	r := httptest.NewRequest(http.MethodPost, "https://sage.databricksapps.com/api/v1/auth/login", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://other.example.com")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation status=%d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "https://sage.databricksapps.com/api/v1/auth/mode", nil)
	r.Header.Set("Origin", "http://localhost:5173")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("App exposes credentialed local-development CORS")
	}
}

func TestDatabricksAppOptionalProtocolsCannotUseLocalTokensOrUnscopedIdentity(t *testing.T) {
	resolver := databricksAppAuthMiddleware(func(context.Context, string) (*auth.User, error) {
		return &auth.User{ID: 1, Email: "owner@example.com", Role: auth.RoleAdmin}, nil
	})
	cfg := config.DefaultConfig()
	h := NewRouterFullRuntime(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, &RuntimeDeps{DatabricksAppAuth: true}, resolver)
	for _, path := range []string{"/mcp", "/mcp/databases/orders", "/api/v1/mcp", "/api/v1/mcp/databases/orders", "/api/v1/mcp/tokens", "/api/v1/agents/test-agent/tokens", "/api/v1/specialist/investigate", "/api/v1/sre/change-events", "/api/v1/sre/sli/orders"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer test-local-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("missing platform identity status=%d", w.Code)
			}
			r.Header.Set("X-Forwarded-Email", "owner@example.com")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "databricks_app_protocol_unsupported") {
				t.Fatalf("unscoped optional protocol status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
