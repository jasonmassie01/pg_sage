package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-02: a session store that cannot answer is 503
// auth_store_unavailable with Retry-After, never the 401 that signs the
// dashboard out. 401 stays for no cookie, an unknown or expired session
// and a cookie that cannot be a session.

// syncBuffer is a log sink safe for concurrent handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSessionStoreLog swaps the store-outage logger for one writing to a
// buffer, with the given clock, for the length of the test.
func captureSessionStoreLog(t *testing.T, now func() time.Time) *syncBuffer {
	t.Helper()
	sink := &syncBuffer{}
	prev := sessionStoreLog
	sessionStoreLog = newRateLimitedLog(sessionStoreLogEvery,
		slog.New(slog.NewTextHandler(sink, nil)))
	if now != nil {
		sessionStoreLog.now = now
	}
	t.Cleanup(func() { sessionStoreLog = prev })
	return sink
}

func wellFormedSessionCookie(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// refusingPool points at a port nothing listens on; pgxpool connects
// lazily, so building it succeeds and the first query fails.
func refusingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=2")
	if err != nil {
		t.Fatalf("building pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func closedAPIPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := refusingPool(t)
	pool.Close()
	return pool
}

func mustNotReach(t *testing.T) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached without a validated session")
	})
}

func callWithCookie(h http.Handler, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sage_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func assertStoreUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body %q: %v", w.Body.String(), err)
	}
	if body["code"] != "auth_store_unavailable" ||
		body["error"] != "session store unavailable" {
		t.Fatalf("body = %v, want the auth_store_unavailable code", body)
	}
}

func assertUnauthorized(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body %q: %v", w.Body.String(), err)
	}
	if body["error"] != msg || body["code"] != "" {
		t.Fatalf("body = %v, want error %q and no code", body, msg)
	}
	if w.Header().Get("Retry-After") != "" {
		t.Fatal("a 401 must not invite a retry")
	}
}

func TestSessionAuth_ClosedPortGives503WithRetryAfter(t *testing.T) {
	sink := captureSessionStoreLog(t, nil)
	h := SessionAuthMiddleware(refusingPool(t))(mustNotReach(t))
	w := callWithCookie(h, "/api/v1/findings", wellFormedSessionCookie(t))
	assertStoreUnavailable(t, w)
	if !strings.Contains(sink.String(), "session store unavailable") {
		t.Fatalf("store outage was not logged: %q", sink.String())
	}
}

func TestSessionAuth_NilPoolWellFormedCookieGives503(t *testing.T) {
	captureSessionStoreLog(t, nil)
	h := SessionAuthMiddleware(nil)(mustNotReach(t))
	assertStoreUnavailable(t, callWithCookie(h, "/api/v1/databases",
		wellFormedSessionCookie(t)))
}

func TestSessionAuth_NoCookieStill401WithBrokenStore(t *testing.T) {
	for name, pool := range map[string]*pgxpool.Pool{
		"nil": nil, "closed": closedAPIPool(t),
	} {
		t.Run(name, func(t *testing.T) {
			h := SessionAuthMiddleware(pool)(mustNotReach(t))
			assertUnauthorized(t, callWithCookie(h, "/api/v1/databases", ""),
				"authentication required")
		})
	}
}

func TestSessionAuth_MalformedCookie401EvenWhenStoreDown(t *testing.T) {
	captureSessionStoreLog(t, nil)
	h := SessionAuthMiddleware(closedAPIPool(t))(mustNotReach(t))
	for _, c := range []string{"'; DROP TABLE sage.sessions; --",
		"invalid-session-id", strings.Repeat("A", 44)} {
		assertUnauthorized(t, callWithCookie(h, "/api/v1/databases", c),
			"session invalid or expired")
	}
}

func TestSessionAuth_StoreOutageLoggedOncePer30s(t *testing.T) {
	clock := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	sink := captureSessionStoreLog(t, func() time.Time { return clock })
	h := SessionAuthMiddleware(closedAPIPool(t))(mustNotReach(t))
	cookie := wellFormedSessionCookie(t)
	for i := 0; i < 100; i++ {
		clock = clock.Add(10 * time.Millisecond) // 100 requests in 1 s
		assertStoreUnavailable(t, callWithCookie(h, "/api/v1/findings", cookie))
	}
	if n := strings.Count(sink.String(), "level=ERROR"); n != 1 {
		t.Fatalf("100 failures in 1 s logged %d times, want 1:\n%s", n, sink.String())
	}
	if !strings.Contains(sink.String(), "closed pool") {
		t.Fatalf("the logged line does not carry the cause: %q", sink.String())
	}
	clock = clock.Add(sessionStoreLogEvery)
	assertStoreUnavailable(t, callWithCookie(h, "/api/v1/findings", cookie))
	if n := strings.Count(sink.String(), "level=ERROR"); n != 2 {
		t.Fatalf("after the window: %d lines, want 2", n)
	}
	if !strings.Contains(sink.String(), "suppressed=99") {
		t.Fatalf("second line does not count the 99 suppressed: %q", sink.String())
	}
}

func TestRateLimitedLog_WindowBoundary(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	clock := start
	sink := &syncBuffer{}
	l := newRateLimitedLog(30*time.Second, slog.New(slog.NewTextHandler(sink, nil)))
	l.now = func() time.Time { return clock }
	if !l.Error("store down") {
		t.Fatal("first call must log")
	}
	clock = start.Add(30*time.Second - time.Nanosecond)
	if l.Error("store down") {
		t.Fatal("a call just inside the window must be suppressed")
	}
	clock = start.Add(30 * time.Second)
	if !l.Error("store down") {
		t.Fatal("a call at the window edge must log")
	}
	if n := strings.Count(sink.String(), "store down"); n != 2 {
		t.Fatalf("logged %d lines, want 2", n)
	}
}

func TestRateLimitedLog_ConcurrentCallsLogOnce(t *testing.T) {
	sink := &syncBuffer{}
	l := newRateLimitedLog(time.Hour, slog.New(slog.NewTextHandler(sink, nil)))
	var wg sync.WaitGroup
	var mu sync.Mutex
	logged := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Error("store down") {
				mu.Lock()
				logged++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if logged != 1 || strings.Count(sink.String(), "store down") != 1 {
		t.Fatalf("50 concurrent calls logged %d (%q), want exactly 1", logged, sink.String())
	}
}

func TestRateLimitedLog_NilLoggerUsesDefault(t *testing.T) {
	sink := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	l := newRateLimitedLog(time.Minute, nil)
	if !l.Error("default sink", "k", "v") || !strings.Contains(sink.String(), "k=v") {
		t.Fatalf("nil logger did not write to slog.Default: %q", sink.String())
	}
}

func TestOAuthAuthorizeLinkIntent_StoreDownGives503(t *testing.T) {
	captureSessionStoreLog(t, nil)
	// intent=link never reaches the login branch, so a zero provider is enough.
	h := oauthAuthorizeRouter(&auth.OAuthProvider{}, closedAPIPool(t))
	path := "/api/v1/auth/oauth/authorize?intent=link"
	assertStoreUnavailable(t, callWithCookie(h, path, wellFormedSessionCookie(t)))
	assertUnauthorized(t, callWithCookie(h, path, ""), "authentication required")
	assertUnauthorized(t, callWithCookie(h, path, "'; DROP"), "authentication required")
}

func TestSessionUser_TriState(t *testing.T) {
	captureSessionStoreLog(t, nil)
	req := func(cookie string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "sage_session", Value: cookie})
		}
		return r
	}
	if u, err := sessionUser(req(""), closedAPIPool(t)); u != nil ||
		!errors.Is(err, auth.ErrSessionInvalid) {
		t.Fatalf("no cookie = (%v, %v), want ErrSessionInvalid", u, err)
	}
	if u, err := sessionUser(req(wellFormedSessionCookie(t)), nil); u != nil ||
		!errors.Is(err, auth.ErrSessionStoreUnavailable) {
		t.Fatalf("nil pool = (%v, %v), want ErrSessionStoreUnavailable", u, err)
	}
	if u, err := sessionUser(req(wellFormedSessionCookie(t)), closedAPIPool(t)); u != nil ||
		!errors.Is(err, auth.ErrSessionStoreUnavailable) {
		t.Fatalf("closed pool = (%v, %v), want ErrSessionStoreUnavailable", u, err)
	}
}

func TestSessionAuth_ClientCancelIsNotLoggedAsOutage(t *testing.T) {
	sink := captureSessionStoreLog(t, nil)
	h := SessionAuthMiddleware(refusingPool(t))(mustNotReach(t))
	cookie := wellFormedSessionCookie(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser navigated away before the lookup finished
	req := httptest.NewRequest(http.MethodGet, "/api/v1/findings", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "sage_session", Value: cookie})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// Still fail closed: an unvalidated request never reaches the handler.
	assertStoreUnavailable(t, w)
	if sink.String() != "" {
		t.Fatalf("a client cancel was logged as a store outage: %q", sink.String())
	}
	// It must not use up the window: the next real failure is logged.
	assertStoreUnavailable(t, callWithCookie(h, "/api/v1/findings", cookie))
	if n := strings.Count(sink.String(), "level=ERROR"); n != 1 {
		t.Fatalf("real outage after a cancel logged %d lines, want 1: %q", n, sink.String())
	}
}
