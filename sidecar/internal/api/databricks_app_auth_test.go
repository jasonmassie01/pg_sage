package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

func TestDatabricksAppAudienceDeniedBeforeDatabaseLookup(t *testing.T) {
	cfg := auth.DatabricksAppConfig{
		AllowedEmailDomains: []string{"databricks.com"}, AdminEmail: "former-admin@external.example",
	}
	called := false
	h := DatabricksAppAuthMiddleware(nil, cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	for _, email := range []string{
		"viewer@external.example", cfg.AdminEmail, "viewer@sub.databricks.com", "viewer@notdatabricks.com",
		"viewer@databricks.com.evil.example",
	} {
		t.Run(email, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			r.Header.Set("X-Forwarded-Email", email)
			r.Header.Set("X-Forwarded-Role", "admin")
			r.Header.Set("Authorization", "Bearer old-local-token")
			r.AddCookie(&http.Cookie{Name: "sage_session", Value: "old-admin-session"})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var response map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusForbidden || response["code"] != "databricks_app_audience_denied" || !strings.Contains(response["error"], "organization") || called {
				t.Fatalf("audience denial = %d %s handler=%v", w.Code, w.Body.String(), called)
			}
			if strings.Contains(w.Body.String(), "account is unavailable") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("audience denial reached nil account store or was cacheable: %s", w.Body.String())
			}
		})
	}
	// A matching mixed-case domain gets past audience checks and reaches the
	// unavailable account store; the domain gate cannot grant a user or role.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("X-Forwarded-Email", "Viewer@DATABRICKS.COM")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "databricks_app_auth_unavailable") || called {
		t.Fatalf("allowed domain bypassed account resolution: %d %s", w.Code, w.Body.String())
	}
}

func TestDatabricksAppAuthDistinguishesRejectionsFromUnavailableStore(t *testing.T) {
	privateConnection := (&url.URL{Scheme: "postgres", Host: "private-host", Path: "/db",
		User: url.UserPassword("private-user", "secret")}).String()
	for _, tc := range []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"wrapped audience refusal", fmt.Errorf("private context: %w", auth.ErrDatabricksAppAudienceDenied), "databricks_app_audience_denied", http.StatusForbidden},
		{"account link required", auth.ErrOAuthLinkRequired, "databricks_app_account_link_required", http.StatusForbidden},
		{"wrapped account link required", fmt.Errorf("private context: %w", auth.ErrOAuthLinkRequired), "databricks_app_account_link_required", http.StatusForbidden},
		{"unverified email", fmt.Errorf("private context: %w", auth.ErrOAuthEmailUnverified), "databricks_app_identity_rejected", http.StatusForbidden},
		{"pool acquire timeout", fmt.Errorf("private context: %w", context.DeadlineExceeded), "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
		{"request canceled", context.Canceled, "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
		{"database capacity", &pgconn.PgError{Code: "53300", Message: "private database capacity detail", Detail: privateConnection}, "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
		{"database connectivity", errors.New(privateConnection + ": connection refused"), "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
		{"untyped refusal text", errors.New(auth.ErrOAuthLinkRequired.Error()), "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
		{"nil user invariant", nil, "databricks_app_auth_unavailable", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := databricksAppAuthMiddleware(func(context.Context, string) (*auth.User, error) {
				if tc.err != nil {
					// Even an accidentally returned administrator must not
					// bypass a simultaneous resolver error.
					return &auth.User{ID: 1, Role: auth.RoleAdmin}, tc.err
				}
				return nil, nil
			})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			r.Header.Set("X-Forwarded-Email", "viewer@example.com")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body["code"] != tc.code || called {
				t.Fatalf("resolver error response = %d %s handler=%v", w.Code, w.Body.String(), called)
			}
			if w.Header().Get("Cache-Control") != "no-store" || len(w.Result().Cookies()) != 0 {
				t.Fatal("failure response was cacheable or issued a local session")
			}
			if tc.status == http.StatusServiceUnavailable {
				if w.Header().Get("Retry-After") != "5" || !strings.Contains(body["error"], "Retry") || !strings.Contains(body["error"], "administrator") {
					t.Fatalf("unavailable response lacks retry guidance: %v %s", w.Header(), w.Body.String())
				}
			} else if w.Header().Get("Retry-After") != "" {
				t.Fatal("identity refusal was presented as an operational retry")
			}
			for _, secret := range []string{"private", "secret", "postgres://", "53300", "oauth:"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatalf("raw resolver detail leaked: %s", w.Body.String())
				}
			}
		})
	}
}

func TestDatabricksAppAuthNeverReusesRoleAfterResolverFailure(t *testing.T) {
	calls := 0
	h := databricksAppAuthMiddleware(func(context.Context, string) (*auth.User, error) {
		calls++
		switch calls {
		case 1:
			return &auth.User{ID: 8, Role: auth.RoleAdmin}, nil
		case 2:
			return nil, context.DeadlineExceeded
		case 3:
			return &auth.User{ID: 8, Role: auth.RoleViewer}, nil
		default:
			return nil, auth.ErrOAuthLinkRequired
		}
	})(RequireRole(auth.RoleAdmin)(http.HandlerFunc(meHandler())))
	for i, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusForbidden, http.StatusForbidden} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		r.Header.Set("X-Forwarded-Email", "owner@example.com")
		r.Header.Set("X-Forwarded-Role", "admin")
		r.AddCookie(&http.Cookie{Name: "sage_session", Value: "old-admin-session"})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status || calls != i+1 {
			t.Fatalf("request %d reused identity/role: status=%d calls=%d body=%s", i+1, w.Code, calls, w.Body.String())
		}
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
