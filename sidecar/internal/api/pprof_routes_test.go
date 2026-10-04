package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// Authenticated pprof (queued follow-up, lesson 2026-10-03: a goroutine
// dump of the dogfood sidecar needed `docker kill -s QUIT`, which stopped
// it). debug.pprof_enabled serves the Go profiler on the API listener at
// /api/v1/debug/pprof/, admin only, behind the same session auth as every
// API route; off by default, and absent (404) when off.

func pprofRouter(t *testing.T, enabled bool, user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Debug.PprofEnabled = enabled
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouter(nil, cfg, nil, inject)
}

func pprofGet(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestPprof_DisabledByDefaultIsNotFound(t *testing.T) {
	h := pprofRouter(t, false, testAdminUser())
	for _, p := range []string{"/api/v1/debug/pprof/", "/api/v1/debug/pprof/goroutine",
		"/api/v1/debug/pprof/profile?seconds=1"} {
		if w := pprofGet(h, p); w.Code != http.StatusNotFound {
			t.Errorf("%s with pprof off: %d, want 404", p, w.Code)
		}
	}
}

func TestPprof_AdminReadsProfiles(t *testing.T) {
	h := pprofRouter(t, true, testAdminUser())
	w := pprofGet(h, "/api/v1/debug/pprof/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "goroutine") {
		t.Fatalf("index: %d %q", w.Code, w.Body.String())
	}
	w = pprofGet(h, "/api/v1/debug/pprof/goroutine?debug=1")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "goroutine profile") {
		t.Fatalf("goroutine dump: %d %.200q", w.Code, w.Body.String())
	}
	for _, p := range []string{"heap", "allocs", "block", "mutex", "threadcreate",
		"cmdline", "symbol"} {
		w := pprofGet(h, "/api/v1/debug/pprof/"+p)
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s: %d with %d bytes", p, w.Code, w.Body.Len())
		}
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store (profiles are sensitive)", cc)
	}
}

func TestPprof_CPUProfileAndTraceAreBounded(t *testing.T) {
	h := pprofRouter(t, true, testAdminUser())
	if w := pprofGet(h, "/api/v1/debug/pprof/profile?seconds=1"); w.Code != http.StatusOK ||
		w.Body.Len() == 0 {
		t.Fatalf("1 s CPU profile: %d with %d bytes", w.Code, w.Body.Len())
	}
	if w := pprofGet(h, "/api/v1/debug/pprof/trace?seconds=1"); w.Code != http.StatusOK ||
		w.Body.Len() == 0 {
		t.Fatalf("1 s trace: %d with %d bytes", w.Code, w.Body.Len())
	}
	for _, q := range []string{"seconds=0", "seconds=-1", "seconds=26", "seconds=abc",
		"seconds=1.5"} {
		for _, p := range []string{"profile", "trace"} {
			w := pprofGet(h, "/api/v1/debug/pprof/"+p+"?"+q)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s?%s: %d, want 400", p, q, w.Code)
			}
		}
	}
}

func TestPprof_UnknownProfileIsNotFound(t *testing.T) {
	h := pprofRouter(t, true, testAdminUser())
	for _, p := range []string{"nope", "heap/extra"} {
		if w := pprofGet(h, "/api/v1/debug/pprof/"+p); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	// A dot segment is cleaned by the mux, which redirects to the clean
	// path (outside pprof); it never serves a profile.
	w := pprofGet(h, "/api/v1/debug/pprof/../config")
	if w.Code == http.StatusOK || strings.Contains(w.Header().Get("Location"), "pprof") {
		t.Errorf("dot segment: %d to %q, want no profile", w.Code, w.Header().Get("Location"))
	}
}

func TestPprof_OnlyAdmins(t *testing.T) {
	for name, tc := range map[string]struct {
		user *auth.User
		want int
	}{
		"operator":  {testOperatorUser(), http.StatusForbidden},
		"viewer":    {testViewerUser(), http.StatusForbidden},
		"anonymous": {nil, http.StatusUnauthorized},
	} {
		h := pprofRouter(t, true, tc.user)
		for _, p := range []string{"/api/v1/debug/pprof/", "/api/v1/debug/pprof/heap",
			"/api/v1/debug/pprof/profile?seconds=1"} {
			if w := pprofGet(h, p); w.Code != tc.want {
				t.Errorf("%s %s: %d, want %d", name, p, w.Code, tc.want)
			}
		}
	}
}

// Through the real session middleware: no session cookie is refused
// before the handler, and an MCP bearer token (accepted only on the MCP
// endpoint) is not a way in either.
func TestPprof_SessionMiddlewareGuardsIt(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Debug.PprofEnabled = true
	h := NewRouter(nil, cfg, nil, SessionAuthMiddleware(nil))
	w := pprofGet(h, "/api/v1/debug/pprof/heap")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d, want 401", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/debug/pprof/heap", nil)
	req.Header.Set("Authorization", "Bearer sage_mcp_0123456789abcdef")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("MCP bearer token: %d, want 401", w.Code)
	}
	if shouldSkipAuth("/api/v1/debug/pprof/") || shouldSkipAuth("/api/v1/debug/pprof/heap") {
		t.Fatal("pprof must never be on the auth skip list")
	}
}
