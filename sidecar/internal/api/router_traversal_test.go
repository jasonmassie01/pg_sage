package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The session middleware's skip decision reads the request path before
// any routing. A path that only looks like a skipped prefix ("..", an
// encoded dot or slash) must still need a session, and through the full
// router it must never reach a gated handler.

var traversalPaths = []string{
	"/api/v1/specialist/../users",
	"/api/v1/specialist/%2e%2e/users",
	"/api/v1/specialist/%2E%2E/users",
	"/api/v1/mcp%2F..%2Fusers",
	"/api/v1/mcp/databases/../../users",
	"/api/v1/auth/login/../../users",
	"/api/v1/sre/sli/../../users",
	"/assets/../api/v1/users",
	"/api/v1/specialist/./../users",
}

func TestTraversal_SessionSkipNeedsCanonicalPath(t *testing.T) {
	for _, path := range traversalPaths {
		reached := false
		h := SessionAuthMiddleware(nil)(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { reached = true }))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if reached || rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: status %d reached=%v, want 401 and not reached",
				path, rec.Code, reached)
		}
	}
}

func TestTraversal_CanonicalSkipPathsStillSkip(t *testing.T) {
	for _, path := range []string{
		"/api/v1/auth/login", "/api/v1/auth/mode", "/api/v1/mcp",
		"/api/v1/mcp/databases/orders", "/api/v1/specialist/v1/ask",
		"/api/v1/sre/change-events", "/api/v1/sre/sli/latency", "/health",
		"/assets/app.js", "/findings",
	} {
		reached := false
		h := SessionAuthMiddleware(nil)(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) { reached = true }))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		if !reached {
			t.Errorf("%s: session middleware refused a path it must leave to its handler", path)
		}
	}
}

func TestTraversal_SkipDecisionTable(t *testing.T) {
	cases := map[string]bool{
		"/api/v1/specialist/x":         true,
		"/api/v1/specialist/../users":  false,
		"/api/v1/specialist//x":        false,
		"/api/v1/users":                false,
		"/api/v1/auth/login":           true,
		"/api/v1/auth/login/":          false,
		"/api/v1/sre/sli/a/../../user": false,
		"":                             false,
	}
	for path, want := range cases {
		r := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: path}}
		if got := skipsSessionAuth(r); got != want {
			t.Errorf("skipsSessionAuth(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTraversal_FullRouterNeverReachesGatedHandler(t *testing.T) {
	h := buildOptionalRouter(t, charSessionAuth)
	for _, path := range traversalPaths {
		code, body, final := serveFollowingRedirect(h, path)
		if code != http.StatusUnauthorized && code != http.StatusNotFound {
			t.Errorf("%s (served as %s): status %d %.80q, want 401 or 404",
				path, final, code, body)
		}
	}
}

// serveFollowingRedirect serves path without a session and follows one
// same-host redirect (ServeMux cleans ".." with a 301).
func serveFollowingRedirect(h http.Handler, path string) (int, string, string) {
	serve := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	rec := serve(path)
	if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusTemporaryRedirect ||
		rec.Code == http.StatusPermanentRedirect {
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Host != "" {
			return -2, "redirect off host: " + rec.Header().Get("Location"), path
		}
		rec = serve(loc.RequestURI())
		return rec.Code, rec.Body.String(), loc.RequestURI()
	}
	return rec.Code, rec.Body.String(), path
}
