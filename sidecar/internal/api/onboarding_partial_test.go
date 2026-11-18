package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// One unreachable database must not blank Getting started and First look
// for the rest of the fleet (firstlook-03).

// readTracker counts the fake's reads in flight and the shortest time a
// read had left before its context deadline.
type readTracker struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	reads    int
	minLeft  time.Duration
	noBound  bool
}

func (r *readTracker) enter(ctx context.Context) func() {
	r.mu.Lock()
	r.inFlight++
	r.reads++
	r.peak = max(r.peak, r.inFlight)
	if deadline, ok := ctx.Deadline(); !ok {
		r.noBound = true
	} else if left := time.Until(deadline); r.minLeft == 0 || left < r.minLeft {
		r.minLeft = left
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}
}

func twoDatabaseFake() *fakeOnboardingReader {
	f := newFakeOnboarding()
	a := f.dbs["app"]
	a.Name = "a"
	f.names = []string{"a", "b"}
	f.dbs = map[string]fakeDB{"a": a,
		"b": {Name: "b", Connected: false, TrustLevel: "advisory"}}
	return f
}

func entryNamed(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	for _, e := range body["databases"].([]any) {
		if m := e.(map[string]any); m["database"] == name {
			return m
		}
	}
	t.Fatalf("no entry %q in %v", name, body)
	return nil
}

// captureLog routes slog to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestOnboardingHandlerKeepsOtherDatabasesWhenOneFails(t *testing.T) {
	logs := captureLog(t)
	f := twoDatabaseFake()
	f.errs = map[string]error{
		"b": errors.New("pq: password authentication failed for user sage")}
	rec, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 with a per-entry error: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("the cause leaked into the body: %s", rec.Body)
	}
	a := entryNamed(t, body, "a")
	if _, has := a["error"]; has || a["connected"] != true ||
		a["first_look"].(map[string]any)["items"] != float64(1) ||
		len(a["steps"].([]any)) != 7 {
		t.Fatalf("healthy entry = %v", a)
	}
	b := entryNamed(t, body, "b")
	if b["connected"] != false || b["error"] != "read_failed" ||
		b["trust_level"] != "advisory" || len(b["steps"].([]any)) != 7 {
		t.Fatalf("failed entry = %v", b)
	}
	connected := onboardingStep(t, b, onboarding.StepConnected)
	if connected["done"] != false ||
		!strings.Contains(connected["detail"].(string), "cannot reach the database") {
		t.Fatalf("connected step of the failed entry = %v", connected)
	}
	got := logs.String()
	if !strings.Contains(got, "database=b") ||
		!strings.Contains(got, "password authentication failed") ||
		!strings.Contains(got, "read_failed") {
		t.Fatalf("server log lacks the database and cause: %s", got)
	}
}

func TestFirstLookHandlerKeepsOtherDatabasesWhenOneFails(t *testing.T) {
	f := twoDatabaseFake()
	f.errs = map[string]error{
		"b": errors.New("pq: password authentication failed for user sage")}
	rec, body := obGet(t, firstLookHandler(f), "/api/v1/first-look")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("first look = %d %s", rec.Code, rec.Body)
	}
	a := entryNamed(t, body, "a")
	if _, has := a["error"]; has || a["report"] == nil ||
		len(a["report"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("healthy entry = %v", a)
	}
	b := entryNamed(t, body, "b")
	if b["report"] != nil || b["error"] != "read_failed" {
		t.Fatalf("failed entry = %v", b)
	}
}

// An unreachable database is reported as disconnected even when the last
// status snapshot still says connected: the read just failed to reach it.
func TestOnboardingHandlerUnreachableIsDisconnected(t *testing.T) {
	f := twoDatabaseFake()
	b := f.dbs["b"]
	b.Connected = true
	f.dbs["b"] = b
	f.errs = map[string]error{"b": &net.OpError{Op: "dial", Net: "tcp",
		Err: errors.New("connect: connection refused")}}
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	entry := entryNamed(t, body, "b")
	step := onboardingStep(t, entry, onboarding.StepConnected)
	if entry["error"] != "unreachable" || entry["connected"] != false ||
		step["done"] != false {
		t.Fatalf("unreachable entry = %v", entry)
	}
}

func TestEntryErrorCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("database %q: %w", "x", errNoRuntime), entryNoRuntime},
		{fmt.Errorf("read: %w", context.DeadlineExceeded), entryUnreachable},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, entryUnreachable},
		{fmt.Errorf("read: %w", &net.DNSError{Err: "no such host", Name: "db"}),
			entryUnreachable},
		{errors.New(`relation "sage.first_look" does not exist`), entryReadFailed},
		{fmt.Errorf("read: %w", context.Canceled), entryReadFailed},
	}
	for _, c := range cases {
		if got := entryErrorCode(c.err); got != c.want {
			t.Errorf("entryErrorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestOnboardingHandlerEmptyFleet(t *testing.T) {
	f := newFakeOnboarding()
	f.names = []string{}
	for path, h := range map[string]http.Handler{
		"/api/v1/onboarding": onboardingHandler(f), "/api/v1/first-look": firstLookHandler(f)} {
		rec, _ := obGet(t, h, path)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"databases":[]}` {
			t.Fatalf("%s on an empty fleet = %d %q", path, rec.Code, rec.Body)
		}
	}
	if f.track.reads != 0 {
		t.Fatalf("an empty fleet read %d databases", f.track.reads)
	}
}

// A registered database whose connection failed at startup (nil pool) has
// no runtime: it is listed with that code, its snapshot status kept.
func TestFleetOnboardingReaderNilPool(t *testing.T) {
	cfg := &config.Config{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "down",
		Status: &fleet.InstanceStatus{Connected: false, TrustLevel: "observation"}})
	reader := newFleetOnboardingReader(mgr, cfg, nil, nil)
	rec, body := obGet(t, onboardingHandler(reader), "/api/v1/onboarding")
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	entry := entryNamed(t, body, "down")
	if entry["error"] != "no_runtime" || entry["connected"] != false ||
		entry["trust_level"] != "observation" {
		t.Fatalf("nil-pool entry = %v", entry)
	}
	_, body = obGet(t, firstLookHandler(reader), "/api/v1/first-look?database=down")
	entry = entryNamed(t, body, "down")
	if entry["error"] != "no_runtime" || entry["report"] != nil {
		t.Fatalf("nil-pool first look = %v", entry)
	}
	// The trust guide still needs a runtime and still fails as a whole.
	if rec, _ := obGet(t, trustGuideHandler(reader),
		"/api/v1/onboarding/trust?database=down"); rec.Code != 500 {
		t.Fatalf("trust guide without a runtime = %d", rec.Code)
	}
}

func slowFake() *fakeOnboardingReader {
	f := twoDatabaseFake()
	c := f.dbs["a"]
	c.Name = "c"
	f.dbs["c"] = c
	f.names = []string{"a", "b", "c"}
	b := f.dbs["b"]
	b.Connected = true
	f.dbs["b"] = b
	f.block = map[string]bool{"b": true}
	return f
}

// TestOnboardingSlowDatabase: a read that waits until its context ends is
// cut at the per-database bound; the other entries are served.
func TestOnboardingSlowDatabase(t *testing.T) {
	t.Run("onboarding", func(t *testing.T) {
		t.Parallel()
		f := slowFake()
		start := time.Now()
		rec, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
		assertBounded(t, time.Since(start), rec.Code)
		for _, name := range []string{"a", "c"} {
			e := entryNamed(t, body, name)
			if _, has := e["error"]; has ||
				e["first_look"].(map[string]any)["ready"] != true {
				t.Fatalf("entry %s = %v", name, e)
			}
		}
		if b := entryNamed(t, body, "b"); b["error"] != "unreachable" ||
			b["connected"] != false {
			t.Fatalf("slow entry = %v", b)
		}
	})
	t.Run("first look", func(t *testing.T) {
		t.Parallel()
		f := slowFake()
		start := time.Now()
		rec, body := obGet(t, firstLookHandler(f), "/api/v1/first-look")
		assertBounded(t, time.Since(start), rec.Code)
		if entryNamed(t, body, "a")["report"] == nil ||
			entryNamed(t, body, "c")["report"] == nil ||
			entryNamed(t, body, "b")["error"] != "unreachable" {
			t.Fatalf("first look = %v", body)
		}
	})
}

func assertBounded(t *testing.T, took time.Duration, code int) {
	t.Helper()
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if took >= 3500*time.Millisecond || took < onboardingReadTimeout-100*time.Millisecond {
		t.Fatalf("response took %v, want the %v per-database bound", took,
			onboardingReadTimeout)
	}
}

// Reads fan out, but never on more than onboardingWorkers goroutines, and
// every read carries the per-database deadline. Entries keep name order.
func TestOnboardingFanOutIsBounded(t *testing.T) {
	f := newFakeOnboarding()
	base := f.dbs["app"]
	f.names, f.dbs = nil, map[string]fakeDB{}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("db%02d", i)
		d := base
		d.Name = name
		f.names, f.dbs[name] = append(f.names, name), d
	}
	f.delay = 40 * time.Millisecond
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	dbs := body["databases"].([]any)
	if len(dbs) != 20 {
		t.Fatalf("entries = %d", len(dbs))
	}
	for i, e := range dbs {
		if want := fmt.Sprintf("db%02d", i); e.(map[string]any)["database"] != want {
			t.Fatalf("entry %d = %v, want %s (name order)", i, e, want)
		}
	}
	tr := &f.track
	if tr.reads != 20 || tr.peak > onboardingWorkers || tr.peak < 2 {
		t.Fatalf("reads %d, peak in flight %d, want 20 reads on 2..%d workers", tr.reads,
			tr.peak, onboardingWorkers)
	}
	if tr.noBound || tr.minLeft > onboardingReadTimeout ||
		tr.minLeft < onboardingReadTimeout-time.Second {
		t.Fatalf("read deadline: unbounded=%v shortest left %v, want about %v", tr.noBound,
			tr.minLeft, onboardingReadTimeout)
	}
}

// State transition: a database that comes back is served whole on the
// next poll, with no error left over from the failed one.
func TestOnboardingRecoversWhenTheDatabaseReturns(t *testing.T) {
	f := twoDatabaseFake()
	f.errs = map[string]error{"b": context.DeadlineExceeded}
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	if entryNamed(t, body, "b")["error"] != "unreachable" {
		t.Fatalf("down entry = %v", entryNamed(t, body, "b"))
	}
	f.errs = nil
	b := f.dbs["b"]
	b.Connected = true
	f.dbs["b"] = b
	_, body = obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	entry := entryNamed(t, body, "b")
	if _, has := entry["error"]; has || entry["connected"] != true {
		t.Fatalf("recovered entry = %v", entry)
	}
}
