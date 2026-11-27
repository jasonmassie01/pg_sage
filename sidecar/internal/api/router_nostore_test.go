package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// apiauth-12: securityHeadersMiddleware sets Cache-Control: no-store on
// every /api/v1 response, whatever answered it: a handler, a gate, the
// mux, or a check that runs before routing.
func TestRouterChainNoStore_EveryAPIResponse(t *testing.T) {
	h := chainRouter(t, &chainProbe{}, false)
	cases := []struct {
		name, method, path, contentType, origin string
		wantStatus                              int
	}{
		{"handler 200", http.MethodGet, "/api/v1/databases", "", "", http.StatusOK},
		{"unknown route 404", http.MethodGet, "/api/v1/no-such-route", "", "",
			http.StatusNotFound},
		{"mux 405", http.MethodDelete, "/api/v1/events", "", "", http.StatusMethodNotAllowed},
		{"JSON check 415", http.MethodPost, "/api/v1/emergency-stop", "text/plain", "",
			http.StatusUnsupportedMediaType},
		{"cross-origin 403", http.MethodPost, "/api/v1/emergency-stop", "application/json",
			"https://evil.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := chainServe(h, tc.method, tc.path, tc.contentType, tc.origin)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s = %d, want %d: %s", tc.method, tc.path, rec.Code,
					tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("existing header changed: X-Frame-Options = %q", got)
			}
		})
	}
}

// An unauthenticated caller's 401 carries it too.
func TestRouterChainNoStore_UnauthenticatedRefusal(t *testing.T) {
	h := NewRouter(nil, config.DefaultConfig(), nil)
	rec := chainServe(h, http.MethodGet, "/api/v1/databases", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("401 Cache-Control = %q, want no-store", got)
	}
}

// Scope: the dashboard and liveness endpoint outside /api/v1 keep their
// own caching.
func TestRouterChainNoStore_NotOnStaticRoutes(t *testing.T) {
	h := chainRouter(t, &chainProbe{}, false)
	for _, path := range []string{"/health", "/"} {
		rec := chainServe(h, http.MethodGet, path, "", "")
		if got := rec.Header().Get("Cache-Control"); got == "no-store" {
			t.Errorf("%s got the API's no-store", path)
		}
	}
}

// The SSO link grant is a bearer secret: its 201 must not be cached. DB.
func TestOAuthLinkGrantIssueNoStore(t *testing.T) {
	f := newLinkFixture(t)
	f.login(t, auth.RoleAdmin)
	target, _ := f.createUser(t, "target", auth.RoleOperator)
	resp := f.raw(t, http.MethodPost,
		"/api/v1/users/"+strconv.Itoa(target)+"/oidc-link-grant", "{}")
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(data), `"token"`) {
		t.Fatalf("issue grant: %d %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("link grant Cache-Control = %q, want no-store", got)
	}
}
