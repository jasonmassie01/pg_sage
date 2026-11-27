package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrency test: the entry is written by the inner layer and read by
// the outer one on the same request goroutine; nothing is shared between
// requests.

// captureAccessLog routes slog to a buffer for the test.
func captureAccessLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func accessLines(buf *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="api access"`) {
			out = append(out, line)
		}
	}
	return out
}

func fakeUserMiddleware(id int, role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := &auth.User{ID: id, Email: "fixture@example.com", Role: role}
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), userContextKey, user)))
		})
	}
}

func accessRouter(
	t *testing.T, accessLog *bool, mw ...func(http.Handler) http.Handler,
) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.API.AccessLog = accessLog
	return newTestRouter(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, nil, mw...)
}

func accessServe(h http.Handler, method, target, body, origin string) int {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAccessLog_ViewerForbiddenPostIsLoggedWithoutBodyOrQuery(t *testing.T) {
	buf := captureAccessLog(t)
	h := accessRouter(t, nil, fakeUserMiddleware(42, auth.RoleViewer))
	code := accessServe(h, http.MethodPost, "/api/v1/emergency-stop?token=query-secret",
		`{"reason":"body-secret"}`, "")
	if code != http.StatusForbidden {
		t.Fatalf("viewer POST = %d, want 403", code)
	}
	lines := accessLines(buf)
	if len(lines) != 1 {
		t.Fatalf("access lines = %d, want 1:\n%s", len(lines), buf.String())
	}
	for _, want := range []string{"method=POST", `pattern="POST /api/v1/emergency-stop"`,
		"user_id=42", "role=viewer", "status=403", "latency_ms="} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access line omits %s: %s", want, lines[0])
		}
	}
	for _, secret := range []string{"body-secret", "query-secret", "token="} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("access log leaked %q: %s", secret, buf.String())
		}
	}
}

func TestAccessLog_SuccessfulReadIsNotLogged(t *testing.T) {
	buf := captureAccessLog(t)
	h := accessRouter(t, nil, fakeUserMiddleware(1, auth.RoleAdmin))
	if code := accessServe(h, http.MethodGet, "/api/v1/databases", "", ""); code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	if lines := accessLines(buf); len(lines) != 0 {
		t.Fatalf("GET 200 was logged: %v", lines)
	}
}

// Mutating requests are logged whatever their status, including ones a
// check before routing refused.
func TestAccessLog_MutatingRequestRefusedBeforeRoutingIsLoggedByPath(t *testing.T) {
	buf := captureAccessLog(t)
	h := accessRouter(t, nil, fakeUserMiddleware(1, auth.RoleAdmin))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/emergency-stop", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	lines := accessLines(buf)
	if rec.Code != http.StatusUnsupportedMediaType || len(lines) != 1 {
		t.Fatalf("code %d, lines %v", rec.Code, lines)
	}
	if !strings.Contains(lines[0], "path=/api/v1/emergency-stop") ||
		!strings.Contains(lines[0], "status=415") || strings.Contains(lines[0], "user_id") {
		t.Fatalf("pre-routing line = %s", lines[0])
	}
}

// 401 from the gate and 403 from the cross-origin check are logged even for
// reads, and the unauthenticated one has no user.
func TestAccessLog_RefusalsAreLoggedForReads(t *testing.T) {
	buf := captureAccessLog(t)
	anon := accessRouter(t, nil)
	if code := accessServe(anon, http.MethodGet, "/api/v1/databases", "", ""); code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d, want 401", code)
	}
	admin := accessRouter(t, nil, fakeUserMiddleware(1, auth.RoleAdmin))
	if code := accessServe(admin, http.MethodPost, "/api/v1/emergency-stop", "{}",
		"https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", code)
	}
	lines := accessLines(buf)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "status=401") ||
		!strings.Contains(lines[0], `pattern="GET /api/v1/databases"`) ||
		strings.Contains(lines[0], "user_id") {
		t.Errorf("401 line = %s", lines[0])
	}
	if !strings.Contains(lines[1], "status=403") ||
		!strings.Contains(lines[1], "path=/api/v1/emergency-stop") {
		t.Errorf("cross-origin line = %s", lines[1])
	}
}

func TestAccessLog_DisabledLogsNothing(t *testing.T) {
	buf := captureAccessLog(t)
	off := false
	h := accessRouter(t, &off, fakeUserMiddleware(42, auth.RoleViewer))
	if code := accessServe(h, http.MethodPost, "/api/v1/emergency-stop", "{}", ""); code !=
		http.StatusForbidden {
		t.Fatalf("viewer POST = %d, want 403", code)
	}
	if lines := accessLines(buf); len(lines) != 0 {
		t.Fatalf("access_log:false still logged: %v", lines)
	}
	on := true
	h = accessRouter(t, &on, fakeUserMiddleware(42, auth.RoleViewer))
	accessServe(h, http.MethodPost, "/api/v1/emergency-stop", "{}", "")
	if lines := accessLines(buf); len(lines) != 1 {
		t.Fatalf("access_log:true lines = %d, want 1", len(lines))
	}
}

func TestAccessLogged_Boundaries(t *testing.T) {
	cases := []struct {
		method string
		status int
		want   bool
	}{
		{http.MethodGet, 200, false}, {http.MethodGet, 404, false},
		{http.MethodGet, 500, false}, {http.MethodHead, 401, true},
		{http.MethodGet, 403, true}, {http.MethodOptions, 204, false},
		{http.MethodPost, 201, true}, {http.MethodPut, 200, true},
		{http.MethodPatch, 500, true}, {http.MethodDelete, 204, true},
	}
	for _, tc := range cases {
		if got := accessLogged(tc.method, tc.status); got != tc.want {
			t.Errorf("accessLogged(%s, %d) = %v, want %v", tc.method, tc.status, got, tc.want)
		}
	}
}

// The status writer keeps streaming working: the SSE handler needs a
// Flusher, and an implicit 200 is recorded.
func TestAccessStatusWriter_FlushAndImplicitStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &accessStatusWriter{ResponseWriter: rec}
	var _ http.Flusher = sw
	sw.Flush()
	if sw.status != http.StatusOK || !rec.Flushed {
		t.Fatalf("flush: status %d, flushed %v", sw.status, rec.Flushed)
	}
	sw.WriteHeader(http.StatusTeapot) // after the implicit 200: ignored
	if sw.status != http.StatusOK {
		t.Fatalf("status overwritten: %d", sw.status)
	}
	if sw.Unwrap() != rec {
		t.Fatal("Unwrap lost the underlying writer")
	}
}

// Nil: a request that never went through the outer layer (no entry in the
// context) passes the inner layer untouched.
func TestAccessStamp_NoEntryIsANoop(t *testing.T) {
	called := false
	h := accessStampMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	if !called {
		t.Fatal("inner handler not called")
	}
}

func TestAccessLog_LongPathIsCapped(t *testing.T) {
	buf := captureAccessLog(t)
	h := accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	long := "/api/v1/" + strings.Repeat("a", 500)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, long, nil))
	lines := accessLines(buf)
	if len(lines) != 1 || strings.Contains(lines[0], strings.Repeat("a", accessLogMaxPath)) {
		t.Fatalf("long path not capped: %v", lines)
	}
}
