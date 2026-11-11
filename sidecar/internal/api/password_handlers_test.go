package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-03 without a database: the forced-change gate's allow list, the
// handlers' refusals before any store call, and the Databricks Apps mode,
// which registers no local password routes.

// No concurrency test here: the allow list is a pure function; the racing
// change is covered on real PostgreSQL in the auth package.

func TestPasswordChangeGate_AllowList(t *testing.T) {
	cases := []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/api/v1/auth/me", true},
		{"PUT", "/api/v1/auth/password", true},
		{"POST", "/api/v1/auth/logout", true},
		{"GET", "/api/v1/cases", false},
		{"GET", "/api/v1/auth/password", true},
		{"DELETE", "/api/v1/auth/sessions", false},
		{"GET", "/api/v1/auth/me/", false},
		{"GET", "/api/v1/auth/password/../../cases", false},
		{"POST", "/api/v1/users/7/password-reset", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://x"+c.path, nil)
		if got := allowedDuringPasswordChange(r); got != c.allowed {
			t.Fatalf("%s %s allowed = %v, want %v", c.method, c.path, got, c.allowed)
		}
	}
}

func TestPasswordChangeGate_BlocksFlaggedUserOnly(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := requirePasswordChanged(inner)
	cases := []struct {
		user *auth.User
		path string
		want int
	}{
		{&auth.User{ID: 4, MustChangePassword: true}, "/api/v1/cases", http.StatusForbidden},
		{&auth.User{ID: 4, MustChangePassword: true}, "/api/v1/auth/me", http.StatusTeapot},
		{&auth.User{ID: 4}, "/api/v1/cases", http.StatusTeapot},
		{nil, "/api/v1/cases", http.StatusTeapot},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.user != nil {
			req = withUser(req, c.user)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Fatalf("user %+v %s = %d, want %d", c.user, c.path, w.Code, c.want)
		}
		if c.want == http.StatusForbidden &&
			!strings.Contains(w.Body.String(), "password_change_required") {
			t.Fatalf("forced refusal body = %s, want password_change_required", w.Body)
		}
	}
}

func TestChangePasswordHandler_RefusesBeforeTheStore(t *testing.T) {
	// A nil pool would panic if the handler reached the store.
	h := changePasswordHandler(nil)
	if w := doRequest(h, http.MethodPut, "/api/v1/auth/password",
		`{"current_password":"a","new_password":"b"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session user = %d, want 401", w.Code)
	}
	user := &auth.User{ID: 9, Email: "nine@x.io", HasPassword: true}
	for _, body := range []string{``, `{}`, `[1]`, `{"current_password":"x"}`,
		`{"current_password":"long-enough-current","new_password":"short"}`,
		`{"current_password":"long-enough-current","new_password":"NINE@x.io"}`} {
		w := doRequestWithUser(h, http.MethodPut, "/api/v1/auth/password", body, user)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q = %d %s, want 400", body, w.Code, w.Body)
		}
	}
	sso := &auth.User{ID: 9, Email: "nine@x.io"}
	w := doRequestWithUser(h, http.MethodPut, "/api/v1/auth/password",
		`{"new_password":"long-enough-new-pw"}`, sso)
	if w.Code != http.StatusConflict {
		t.Fatalf("SSO-only = %d %s, want 409", w.Code, w.Body)
	}
}

func TestResetPasswordHandler_RefusesSelfBeforeTheStore(t *testing.T) {
	h := resetPasswordHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/1/password-reset", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, withUser(req, testAdminUser()))
	if w.Code != http.StatusForbidden {
		t.Fatalf("self reset = %d %s, want 403", w.Code, w.Body)
	}
}

func TestDatabricksAppMode_RegistersNoPasswordRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerDatabricksAppAuthRoutes(mux, nil)
	for _, probe := range [][2]string{{"PUT", "/api/v1/auth/password"},
		{"POST", "/api/v1/users/3/password-reset"}} {
		_, pattern := mux.Handler(httptest.NewRequest(probe[0], probe[1], nil))
		if pattern != "" {
			t.Fatalf("%s %s is registered in Apps mode as %q", probe[0], probe[1], pattern)
		}
	}
}
