package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// --- shouldSkipAuth ---

func TestShouldSkipAuth_LoginPath(t *testing.T) {
	if !shouldSkipAuth("/api/v1/auth/login") {
		t.Error("expected /api/v1/auth/login to skip auth")
	}
}

func TestShouldSkipAuth_HealthPath(t *testing.T) {
	if !shouldSkipAuth("/health") {
		t.Error("expected /health to skip auth")
	}
}

func TestShouldSkipAuth_NonAPIPath(t *testing.T) {
	paths := []string{
		"/", "/dashboard", "/static/app.js", "/favicon.ico",
	}
	for _, p := range paths {
		if !shouldSkipAuth(p) {
			t.Errorf("expected %q to skip auth", p)
		}
	}
}

func TestShouldSkipAuth_APIPaths(t *testing.T) {
	paths := []string{
		"/api/v1/databases",
		"/api/v1/findings",
		"/api/v1/auth/me",
		"/api/v1/auth/logout",
		"/api/v1/config",
	}
	for _, p := range paths {
		if shouldSkipAuth(p) {
			t.Errorf("expected %q to require auth", p)
		}
	}
}

func TestShouldSkipAuth_EmptyPath(t *testing.T) {
	// Empty path does not start with /api/
	if !shouldSkipAuth("") {
		t.Error("expected empty path to skip auth " +
			"(not an API path)")
	}
}

// --- UserFromContext ---

func TestUserFromContext_WithUser(t *testing.T) {
	user := &auth.User{
		ID:    42,
		Email: "test@example.com",
		Role:  auth.RoleAdmin,
	}
	ctx := context.WithValue(
		context.Background(), userContextKey, user)
	got := UserFromContext(ctx)
	if got == nil {
		t.Fatal("expected non-nil user")
	}
	if got.ID != 42 {
		t.Errorf("user ID: got %d, want 42", got.ID)
	}
	if got.Email != "test@example.com" {
		t.Errorf("email: got %q, want test@example.com",
			got.Email)
	}
	if got.Role != auth.RoleAdmin {
		t.Errorf("role: got %q, want admin", got.Role)
	}
}

func TestUserFromContext_WithoutUser(t *testing.T) {
	ctx := context.Background()
	got := UserFromContext(ctx)
	if got != nil {
		t.Errorf("expected nil user, got %+v", got)
	}
}

func TestUserFromContext_WrongType(t *testing.T) {
	ctx := context.WithValue(
		context.Background(), userContextKey, "not a user")
	got := UserFromContext(ctx)
	if got != nil {
		t.Errorf("expected nil for wrong type, got %+v", got)
	}
}

func TestUserFromContext_NilValue(t *testing.T) {
	ctx := context.WithValue(
		context.Background(), userContextKey,
		(*auth.User)(nil))
	got := UserFromContext(ctx)
	if got != nil {
		t.Errorf("expected nil for nil *User, got %+v", got)
	}
}

// --- RequireRole ---

func TestRequireRole_AllowedRole(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := RequireRole("admin")(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req = withUser(req, testAdminUser())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body: got %q, want ok", w.Body.String())
	}
}

func TestRequireRole_DeniedRole(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Error("handler should not be called")
	})

	handler := RequireRole("admin")(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req = withUser(req, testViewerUser())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "insufficient permissions" {
		t.Errorf("error: got %q, want 'insufficient permissions'",
			resp["error"])
	}
}

func TestRequireRole_MultipleRoles(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	})

	handler := RequireRole("admin", "operator")(inner)

	tests := []struct {
		name   string
		user   *auth.User
		status int
	}{
		{"admin allowed", testAdminUser(), 200},
		{"operator allowed", testOperatorUser(), 200},
		{"viewer denied", testViewerUser(), 403},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req = withUser(req, tt.user)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Errorf("status: got %d, want %d",
					w.Code, tt.status)
			}
		})
	}
}

func TestRequireRole_MissingUser(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Error("handler should not be called")
	})

	handler := RequireRole("admin")(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "authentication required" {
		t.Errorf("error: got %q, want 'authentication required'",
			resp["error"])
	}
}

func TestRequireRole_EmptyRoleList(t *testing.T) {
	// No roles specified means nobody is allowed.
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Error("handler should not be called")
	})

	handler := RequireRole()(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	req = withUser(req, testAdminUser())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", w.Code)
	}
}

// --- SessionAuthMiddleware ---

func TestSessionAuthMiddleware_MissingCookie(t *testing.T) {
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Error("handler should not be called")
	})

	// Pass nil pool -- we never reach pool usage because
	// cookie is missing.
	handler := SessionAuthMiddleware(nil)(inner)

	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "authentication required" {
		t.Errorf("error: got %q, want 'authentication required'",
			resp["error"])
	}
}

func TestSessionAuthMiddleware_SkipLogin(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := SessionAuthMiddleware(nil)(inner)

	req := httptest.NewRequest(
		"POST", "/api/v1/auth/login", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called " +
			"for login path")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", w.Code)
	}
}

func TestSessionAuthMiddleware_SkipHealth(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := SessionAuthMiddleware(nil)(inner)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called " +
			"for health path")
	}
}

func TestSessionAuthMiddleware_SkipStaticPaths(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := SessionAuthMiddleware(nil)(inner)

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("expected inner handler to be called " +
			"for non-API path")
	}
}

func TestSessionAuthMiddleware_InvalidSession(t *testing.T) {
	// With a nil pool, ValidateSession will fail, which is
	// the same as an invalid/expired session.
	inner := http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Error("handler should not be called")
	})

	handler := SessionAuthMiddleware(nil)(inner)

	req := httptest.NewRequest(
		"GET", "/api/v1/databases", nil)
	req.AddCookie(&http.Cookie{
		Name:  "sage_session",
		Value: "invalid-session-id",
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "session invalid or expired" {
		t.Errorf("error: got %q, want 'session invalid or "+
			"expired'", resp["error"])
	}
}
