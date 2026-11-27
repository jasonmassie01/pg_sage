package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Characterization of the middleware chain NewRouterFullRuntime builds:
// caller middlewares wrap only the API mux, inside requireJSON, maxBody,
// timeout, the cross-origin check and security headers; /health and the
// SPA sit outside it.

type chainProbe struct {
	sawMiddleware bool
	sawDeadline   bool
}

func chainRouter(t *testing.T, seen *chainProbe, withPool bool) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	mark := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.sawMiddleware = true
			_, seen.sawDeadline = r.Context().Deadline()
			w.Header().Set("X-Char-Mw", "1")
			next.ServeHTTP(w, r)
		})
	}
	// Routes carry their own gate, so the probes need a signed-in caller
	// to reach a handler; fakeAdminMiddleware runs after mark.
	if !withPool {
		return newTestRouter(mgr, cfg, nil, nil, nil, nil, nil, mark, fakeAdminMiddleware)
	}
	return newTestRouter(mgr, cfg, unreachablePool(t), nil, nil, nil, nil, mark,
		fakeAdminMiddleware)
}

func chainServe(
	h http.Handler, method, path, contentType, origin string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRouterChain_APIRoutesGetSecurityHeadersDeadlineAndCallerMiddleware(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, false)
	rec := chainServe(h, http.MethodGet, "/api/v1/databases", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/databases = %d %s", rec.Code, rec.Body.String())
	}
	if !seen.sawMiddleware || !seen.sawDeadline {
		t.Fatalf("caller middleware ran=%v with deadline=%v, want both", seen.sawMiddleware,
			seen.sawDeadline)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
		"Referrer-Policy": "strict-origin-when-cross-origin", "X-Char-Mw": "1",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestRouterChain_EventStreamHasNoDeadline(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, false)
	// An unregistered method still passes the chain (the mux answers 405),
	// so the deadline decision is observed without opening the stream.
	rec := chainServe(h, http.MethodDelete, "/api/v1/events", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/v1/events = %d, want 405", rec.Code)
	}
	if !seen.sawMiddleware || seen.sawDeadline {
		t.Fatalf("events path: middleware=%v deadline=%v, want middleware without deadline",
			seen.sawMiddleware, seen.sawDeadline)
	}
}

func TestRouterChain_JSONCheckRunsBeforeCallerMiddleware(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, false)
	rec := chainServe(h, http.MethodPost, "/api/v1/emergency-stop", "text/plain", "")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON POST = %d, want 415", rec.Code)
	}
	if seen.sawMiddleware {
		t.Fatal("caller middleware ran before the JSON content-type check")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("415 response lost the security headers")
	}
}

func TestRouterChain_PreflightIsNotShortCircuited(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, false)
	rec := chainServe(h, http.MethodOptions, "/api/v1/findings", "",
		"http://localhost:5173")
	if !seen.sawMiddleware {
		t.Fatalf("preflight = %d answered ahead of the API chain", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("dev origin granted CORS: %q", got)
	}
}

func TestRouterChain_HealthAndSPAAreOutsideTheAPIChain(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, false)
	rec := chainServe(h, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("/health = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("/health content type = %q", rec.Header().Get("Content-Type"))
	}
	if seen.sawMiddleware || rec.Header().Get("X-Frame-Options") != "" {
		t.Fatal("/health went through the API middleware chain")
	}
	rec = chainServe(h, http.MethodGet, "/findings/42", "", "")
	if rec.Code != http.StatusOK || seen.sawMiddleware {
		t.Fatalf("SPA fallback = %d middleware=%v", rec.Code, seen.sawMiddleware)
	}
	if !strings.Contains(rec.Body.String(), "<") {
		t.Fatal("SPA fallback did not serve the dashboard index")
	}
}

func TestRouterChain_ChatOpsCallbacksBypassCallerMiddleware(t *testing.T) {
	seen := &chainProbe{}
	h := chainRouter(t, seen, true)
	rec := chainServe(h, http.MethodPost, "/api/v1/chatops/slack/7", "text/plain", "")
	if seen.sawMiddleware {
		t.Fatal("signed chatops callback went through the session middleware")
	}
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatal("chatops callback was subjected to the API JSON check")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("chatops callback lost its security headers")
	}
	// Without a control pool the callbacks are not registered at all.
	bare := chainRouter(t, &chainProbe{}, false)
	if rec := chainServe(bare, http.MethodPost, "/api/v1/chatops/slack/7",
		"application/json", ""); rec.Code == http.StatusOK {
		t.Fatalf("chatops callback answered without a pool: %d", rec.Code)
	}
}
