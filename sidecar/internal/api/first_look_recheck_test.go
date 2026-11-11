package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// firstlook-05: POST /api/v1/first-look/recheck.

type fakeRecheck struct {
	mu    sync.Mutex
	hooks map[string]recheckHook // nil value: the database has no hook
	calls atomic.Int32
}

func (f *fakeRecheck) RecheckHook(name string) (recheckHook, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hook, ok := f.hooks[name]
	if !ok || hook == nil {
		return nil, ok
	}
	return func(ctx context.Context) (firstlook.Report, error) {
		f.calls.Add(1)
		return hook(ctx)
	}, true
}

func (f *fakeRecheck) set(name string, hook recheckHook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[name] = hook
}

func okHook(ctx context.Context) (firstlook.Report, error) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	return firstlook.Report{ID: 9, Database: "app", FinishedAt: at, DurationMS: 120,
		Summary: "One duplicate index.",
		Items: []firstlook.Item{{Rule: firstlook.RuleDuplicateIndex,
			Severity: firstlook.SeverityWarning, Object: "public.i"}},
		Checks: []firstlook.Check{{Rule: firstlook.RuleDuplicateIndex,
			Status: firstlook.CheckFinding}, {Rule: firstlook.RuleSequenceRunway,
			Status: firstlook.CheckDegraded}},
		Capabilities: []firstlook.Capability{{Name: firstlook.CapStatStatements,
			Status: firstlook.CapabilityOK}}}, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func recheckMux(src recheckSource, clock *fakeClock) *http.ServeMux {
	mux := http.NewServeMux()
	registerFirstLookRecheck(mux, src, newRecheckLimiter(firstLookRecheckEvery, clock.Now))
	return mux
}

func postRecheck(t *testing.T, h http.Handler, query string, user *auth.User) (
	*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/first-look/recheck"+query, nil)
	if user != nil {
		req = withUser(req, user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return rec, body
}

func newRecheckFixture() (*fakeRecheck, *fakeClock, *http.ServeMux) {
	src := &fakeRecheck{hooks: map[string]recheckHook{"app": okHook, "other": okHook,
		"starting": nil}}
	clock := &fakeClock{now: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	return src, clock, recheckMux(src, clock)
}

func TestFirstLookRecheck_HappyPathReturnsTheNewSummary(t *testing.T) {
	for _, user := range []*auth.User{testOperatorUser(), testAdminUser()} {
		src, _, mux := newRecheckFixture()
		rec, body := postRecheck(t, mux, "?database=app", user)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code %d body %v, want 200", user.Role, rec.Code, body)
		}
		if body["database"] != "app" {
			t.Fatalf("database = %v, want app", body["database"])
		}
		fl, _ := body["first_look"].(map[string]any)
		if fl["ready"] != true || fl["items"] != float64(1) || fl["checks"] != float64(2) ||
			fl["degraded"] != float64(1) || fl["summary"] != "One duplicate index." ||
			fl["duration_ms"] != float64(120) || fl["finished_at"] == nil {
			t.Fatalf("%s: first_look = %v, want the new report's summary", user.Role, fl)
		}
		if n := src.calls.Load(); n != 1 {
			t.Fatalf("%s: hook calls = %d, want 1", user.Role, n)
		}
	}
}

func TestFirstLookRecheck_InvalidInput(t *testing.T) {
	src, _, mux := newRecheckFixture()
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"?database=ghost", http.StatusNotFound},
		{"?database=all", http.StatusBadRequest},
		{"", http.StatusBadRequest},
		{"?database=" + strings.Repeat("x", 200), http.StatusBadRequest},
		{"?database=a%3Bdrop", http.StatusBadRequest},
	} {
		rec, body := postRecheck(t, mux, tc.query, testOperatorUser())
		if rec.Code != tc.code || body["error"] == "" {
			t.Errorf("%q: code %d body %v, want %d with an error", tc.query, rec.Code,
				body, tc.code)
		}
	}
	rec, body := postRecheck(t, mux, "?database=app", &auth.User{ID: 3,
		Email: "v@test.com", Role: auth.RoleViewer})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: code %d body %v, want 403", rec.Code, body)
	}
	if rec, _ := postRecheck(t, mux, "?database=app", nil); rec.Code !=
		http.StatusUnauthorized {
		t.Fatalf("nobody: code %d, want 401", rec.Code)
	}
	if n := src.calls.Load(); n != 0 {
		t.Fatalf("hook calls = %d on refused requests, want 0", n)
	}
	// None of the refused requests used the database's re-check.
	if rec, body := postRecheck(t, mux, "?database=app", testOperatorUser()); rec.Code !=
		http.StatusOK {
		t.Fatalf("first accepted re-check: code %d body %v, want 200", rec.Code, body)
	}
}

func TestFirstLookRecheck_NoHookIs503AndKeepsTheLimit(t *testing.T) {
	src, _, mux := newRecheckFixture()
	rec, body := postRecheck(t, mux, "?database=starting", testOperatorUser())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d body %v, want 503", rec.Code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "starting") || !strings.Contains(msg, "first look") {
		t.Fatalf("error %q, want the database and why", msg)
	}
	src.set("starting", okHook)
	if rec, body := postRecheck(t, mux, "?database=starting", testOperatorUser()); rec.Code !=
		http.StatusOK {
		t.Fatalf("after the hook appeared: code %d body %v, want 200 (a 503 uses no "+
			"re-check)", rec.Code, body)
	}
}

// A second POST 59 s after the first is refused with the time left; one
// at 60 s runs. The limit is per database.
func TestFirstLookRecheck_LimiterBoundary(t *testing.T) {
	src, clock, mux := newRecheckFixture()
	if rec, _ := postRecheck(t, mux, "?database=app", testOperatorUser()); rec.Code !=
		http.StatusOK {
		t.Fatalf("first: code %d, want 200", rec.Code)
	}
	clock.advance(59 * time.Second)
	rec, body := postRecheck(t, mux, "?database=app", testOperatorUser())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("at 59 s: code %d body %v, want 429", rec.Code, body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if body["retry_after_seconds"] != float64(1) {
		t.Fatalf("retry_after_seconds = %v, want 1", body["retry_after_seconds"])
	}
	if rec, _ := postRecheck(t, mux, "?database=other", testOperatorUser()); rec.Code !=
		http.StatusOK {
		t.Fatalf("another database at 59 s: code %d, want 200", rec.Code)
	}
	clock.advance(time.Second)
	if rec, body := postRecheck(t, mux, "?database=app", testOperatorUser()); rec.Code !=
		http.StatusOK {
		t.Fatalf("at 60 s: code %d body %v, want 200", rec.Code, body)
	}
	if n := src.calls.Load(); n != 3 {
		t.Fatalf("hook calls = %d, want 3", n)
	}
}

func TestRecheckLimiter_RetryAfterRoundsUp(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	lim := newRecheckLimiter(time.Minute, clock.Now)
	if ok, _ := lim.allow("app"); !ok {
		t.Fatal("first call refused")
	}
	clock.advance(500 * time.Millisecond)
	ok, wait := lim.allow("app")
	if ok || wait != 59500*time.Millisecond || retryAfterSeconds(wait) != 60 {
		t.Fatalf("at 0.5 s: ok=%v wait=%s, want refused with 59.5 s, sent as 60", ok, wait)
	}
	if retryAfterSeconds(59500*time.Millisecond) != 60 || retryAfterSeconds(0) != 1 {
		t.Fatal("retry-after must round up and never be 0")
	}
}

func TestFirstLookRecheck_FailureIs502WithTheClass(t *testing.T) {
	src, clock, mux := newRecheckFixture()
	for _, tc := range []struct {
		err   error
		class string
	}{
		{errors.New("decode first look: secret detail"), entryReadFailed},
		{context.DeadlineExceeded, entryUnreachable},
	} {
		src.set("app", func(context.Context) (firstlook.Report, error) {
			return firstlook.Report{}, tc.err
		})
		rec, body := postRecheck(t, mux, "?database=app", testOperatorUser())
		if rec.Code != http.StatusBadGateway || body["class"] != tc.class {
			t.Fatalf("%v: code %d body %v, want 502 class %s", tc.err, rec.Code, body,
				tc.class)
		}
		if strings.Contains(rec.Body.String(), "secret detail") {
			t.Fatalf("502 body leaks the error: %s", rec.Body.String())
		}
		clock.advance(firstLookRecheckEvery)
	}
}

// Two simultaneous POSTs: the limiter admits one, the other gets 429, and
// the hook runs once.
func TestFirstLookRecheck_ConcurrentPostsRunOnce(t *testing.T) {
	src, _, mux := newRecheckFixture()
	release := make(chan struct{})
	src.set("app", func(ctx context.Context) (firstlook.Report, error) {
		<-release
		return okHook(ctx)
	})
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := withUser(httptest.NewRequest(http.MethodPost,
				"/api/v1/first-look/recheck?database=app", nil), testOperatorUser())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusOK] != 1 || got[http.StatusTooManyRequests] != 1 {
		t.Fatalf("codes = %v, want one 200 and one 429", got)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("hook calls = %d, want 1", n)
	}
}

// The fleet source finds the instance's hook; nil without one.
func TestFleetRecheckSource(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "bare",
		Status: &fleet.InstanceStatus{}})
	var ran bool
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "app",
		Status: &fleet.InstanceStatus{},
		FirstLookRecheck: func(context.Context) (firstlook.Report, error) {
			ran = true
			return firstlook.Report{ID: 3}, nil
		}})
	src := fleetRecheckSource{mgr: mgr}
	if hook, found := src.RecheckHook("ghost"); found || hook != nil {
		t.Fatal("unknown database found")
	}
	if hook, found := src.RecheckHook("bare"); !found || hook != nil {
		t.Fatalf("bare: found=%v hook set=%v, want found without a hook", found, hook != nil)
	}
	hook, found := src.RecheckHook("app")
	if !found || hook == nil {
		t.Fatal("app hook missing")
	}
	if r, err := hook(context.Background()); err != nil || r.ID != 3 || !ran {
		t.Fatalf("hook: report %d err %v ran %v", r.ID, err, ran)
	}
	if _, found := (fleetRecheckSource{}).RecheckHook("app"); found {
		t.Fatal("a source without a manager found a database")
	}
}
