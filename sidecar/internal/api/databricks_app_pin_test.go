package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
)

// databricks-07: the middleware passes the single X-Forwarded-User value
// to the resolver beside the email, and maps a changed identity to 403.

type seenIdentity struct {
	calls int
	email string
	user  string
}

func recordingResolver(seen *seenIdentity, role string) func(context.Context, string,
	string) (*auth.User, error) {
	return func(_ context.Context, email, user string) (*auth.User, error) {
		seen.calls++
		seen.email, seen.user = email, user
		return &auth.User{ID: 4, Email: email, Role: role}, nil
	}
}

func TestDatabricksAppForwardedUserPassedToResolver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{"absent", nil, ""},
		{"empty value treated as absent", []string{""}, ""},
		{"single value passed exactly", []string{"8675309"}, "8675309"},
		{"value kept verbatim", []string{" Idp-User "}, " Idp-User "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := &seenIdentity{}
			h := databricksAppAuthMiddleware(recordingResolver(seen, auth.RoleOperator))(
				http.HandlerFunc(meHandler()))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			r.Header.Set("X-Forwarded-Email", "viewer@example.com")
			r.Header["X-Forwarded-User"] = tc.values
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || seen.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, seen.calls, w.Body.String())
			}
			if seen.user != tc.want || seen.email != "viewer@example.com" {
				t.Fatalf("resolver got (%q, %q), want (viewer@example.com, %q)",
					seen.email, seen.user, tc.want)
			}
			var me map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
				t.Fatal(err)
			}
			if me["role"] != auth.RoleOperator {
				t.Fatalf("role = %v, want the persisted operator role", me["role"])
			}
		})
	}
}

func TestDatabricksAppDuplicateForwardedUserRejected(t *testing.T) {
	for _, values := range [][]string{
		{"user-a", "user-b"}, {"user-a", "user-a"}, {"", "user-a"}, {"", ""},
	} {
		t.Run(strings.Join(values, "|"), func(t *testing.T) {
			seen := &seenIdentity{}
			h := databricksAppAuthMiddleware(recordingResolver(seen, auth.RoleAdmin))(
				http.HandlerFunc(meHandler()))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			r.Header.Set("X-Forwarded-Email", "owner@example.com")
			r.Header["X-Forwarded-User"] = values
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || seen.calls != 0 {
				t.Fatalf("duplicate user values: status=%d resolver calls=%d",
					w.Code, seen.calls)
			}
		})
	}
}

func TestDatabricksAppIdentityChangedIs403(t *testing.T) {
	called := false
	h := databricksAppAuthMiddleware(func(context.Context, string, string) (*auth.User, error) {
		return nil, fmt.Errorf("resolve: %w", auth.ErrDatabricksAppIdentityChanged)
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	r.Header.Set("X-Forwarded-Email", "owner@example.com")
	r.Header.Set("X-Forwarded-User", "someone-else")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if called || w.Code != http.StatusForbidden {
		t.Fatalf("called=%v status=%d, want refused with 403", called, w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "databricks_app_identity_changed" ||
		!strings.Contains(body["error"], "administrator") {
		t.Fatalf("body = %v, want code databricks_app_identity_changed", body)
	}
}

func TestDatabricksAppPinResetRejectsInvalidID(t *testing.T) {
	h := resetDatabricksAppPinHandler(nil)
	for _, id := range []string{"0", "-3", "abc", "1.5", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost,
				"/api/v1/users/"+id+"/identity-pin/reset", nil)
			r.SetPathValue("id", id)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("id %q: status=%d, want 400", id, w.Code)
			}
		})
	}
}

func TestDatabricksAppPinResetIsAdminOnly(t *testing.T) {
	cfgRouter := func(role string) http.Handler {
		return databricksAppRouterAs(t, &auth.User{ID: 3, Email: "op@example.com", Role: role})
	}
	for _, role := range []string{auth.RoleViewer, auth.RoleOperator} {
		w := appCall(cfgRouter(role), http.MethodPost, "/api/v1/users/9/identity-pin/reset",
			"op@example.com", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s reset status=%d, want 403", role, w.Code)
		}
	}
}
