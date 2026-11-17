package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fleetpol-02c: the Data API exposure reader caches the one Management
// API read for an hour; every failure, and no token at all, is unknown.
//
// Concurrency: the reader has one writer (its Run goroutine, the cloudtel
// poll) and the posture configuration as reader; the parallel test below
// pins that a read never races a refresh and that one hour makes one call.

// fakeSupabaseAPI answers DataAPISettings with its scripted results in
// turn (the last one repeats) and counts the calls.
type fakeSupabaseAPI struct {
	mu      sync.Mutex
	calls   int
	results []fakeSupabaseResult
}

type fakeSupabaseResult struct {
	schemas []string
	err     error
}

func (f *fakeSupabaseAPI) DataAPISettings(context.Context) (SupabaseDataAPISettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r := f.results[min(f.calls, len(f.results))-1]
	return SupabaseDataAPISettings{Schemas: slices.Clone(r.schemas)}, r.err
}

func (f *fakeSupabaseAPI) set(results ...fakeSupabaseResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results, f.calls = results, 0
}

func (f *fakeSupabaseAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testClock is a settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logLines records the reader's log lines.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) matching(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func newTestDataAPI(api SupabaseAPI) (*SupabaseDataAPI, *testClock, *logLines) {
	clock := &testClock{now: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	logs := &logLines{}
	return NewSupabaseDataAPI(api, SupabaseDataAPIOptions{Now: clock.Now, Logf: logs.logf}),
		clock, logs
}

func requireSchemas(t *testing.T, r *SupabaseDataAPI, want []string) {
	t.Helper()
	got := r.Exposure()
	if want == nil {
		if got != nil {
			t.Fatalf("exposure = %+v, want unknown (nil)", got)
		}
		return
	}
	if got == nil || !slices.Equal(got.Schemas, want) {
		t.Fatalf("exposure = %+v, want schemas %q", got, want)
	}
}

func TestSupabaseDataAPI_HappyPathRecordsTheSchemas(t *testing.T) {
	api := &fakeSupabaseAPI{results: []fakeSupabaseResult{{schemas: []string{"public"}}}}
	r, clock, _ := newTestDataAPI(api)
	requireSchemas(t, r, nil) // nothing read yet: unknown
	r.Refresh(context.Background())
	requireSchemas(t, r, []string{"public"})
	if got := r.Exposure().ReadAt; !got.Equal(clock.Now()) {
		t.Fatalf("ReadAt = %v, want %v", got, clock.Now())
	}
	if api.count() != 1 {
		t.Fatalf("calls = %d, want 1", api.count())
	}
}

// Nil/empty: no API (no token) is unknown for every read and makes no
// call; a nil reader is unknown too.
func TestSupabaseDataAPI_NoAPIIsUnknownWithoutACall(t *testing.T) {
	r, _, logs := newTestDataAPI(nil)
	r.Refresh(context.Background())
	requireSchemas(t, r, nil)
	var typed *fakeSupabaseAPI
	r2, _, _ := newTestDataAPI(typed)
	r2.Refresh(context.Background()) // a typed nil must not be called
	requireSchemas(t, r2, nil)
	var none *SupabaseDataAPI
	none.Refresh(context.Background())
	if none.Exposure() != nil {
		t.Fatal("a nil reader reported an exposure")
	}
	if len(logs.lines) != 0 {
		t.Fatalf("no-token reader logged %q", logs.lines)
	}
}

// Boundary: the one-hour cache. A read at 59 min makes no call, at 60 min
// one call.
func TestSupabaseDataAPI_CachesForOneHour(t *testing.T) {
	api := &fakeSupabaseAPI{results: []fakeSupabaseResult{{schemas: []string{"public"}},
		{schemas: []string{"api"}}}}
	r, clock, _ := newTestDataAPI(api)
	r.Refresh(context.Background())
	clock.advance(59 * time.Minute)
	r.Refresh(context.Background())
	if api.count() != 1 {
		t.Fatalf("calls at 59 min = %d, want 1", api.count())
	}
	requireSchemas(t, r, []string{"public"})
	clock.advance(time.Minute)
	r.Refresh(context.Background())
	if api.count() != 2 {
		t.Fatalf("calls at 60 min = %d, want 2", api.count())
	}
	requireSchemas(t, r, []string{"api"})
}

// Error propagation: 401 and 429 are logged once each and give unknown; a
// 429 backs off an hour.
func TestSupabaseDataAPI_UnauthorizedAndRateLimitedLogOnce(t *testing.T) {
	api := &fakeSupabaseAPI{}
	r, clock, logs := newTestDataAPI(api)
	unauthorized := fmt.Errorf("%w: data API settings HTTP 401", ErrSupabaseUnauthorized)
	limited := fmt.Errorf("%w: data API settings HTTP 429", ErrSupabaseRateLimited)
	api.set(fakeSupabaseResult{err: unauthorized})
	for range 3 {
		r.Refresh(context.Background())
		requireSchemas(t, r, nil)
		clock.advance(time.Hour)
	}
	if n := logs.matching("HTTP 401"); n != 1 {
		t.Fatalf("401 logged %d times, want once: %q", n, logs.lines)
	}
	api.set(fakeSupabaseResult{err: limited})
	r.Refresh(context.Background())
	clock.advance(59 * time.Minute)
	r.Refresh(context.Background())
	if api.count() != 1 {
		t.Fatalf("calls within the 429 back-off = %d, want 1", api.count())
	}
	clock.advance(time.Minute)
	r.Refresh(context.Background())
	if api.count() != 2 || logs.matching("HTTP 429") != 1 {
		t.Fatalf("after the back-off: calls = %d, 429 logs = %d; want 2 and 1",
			api.count(), logs.matching("HTTP 429"))
	}
	requireSchemas(t, r, nil)
	if n := logs.matching("HTTP 401"); n != 1 {
		t.Fatalf("401 logged %d times after a 429, want still once", n)
	}
}

// A success re-arms the log, so the next failure is reported again.
func TestSupabaseDataAPI_SuccessRearmsTheFailureLog(t *testing.T) {
	api := &fakeSupabaseAPI{}
	r, clock, logs := newTestDataAPI(api)
	unauthorized := fmt.Errorf("%w: data API settings HTTP 401", ErrSupabaseUnauthorized)
	for _, res := range []fakeSupabaseResult{{err: unauthorized},
		{schemas: []string{"public"}}, {err: unauthorized}} {
		api.set(res)
		r.Refresh(context.Background())
		clock.advance(time.Hour)
	}
	if n := logs.matching("HTTP 401"); n != 2 {
		t.Fatalf("401 logged %d times around a success, want 2: %q", n, logs.lines)
	}
}

// State transition: a failure after a known state is unknown again (never
// a stale "not exposed"); a malformed response is unknown and logged.
func TestSupabaseDataAPI_FailureAfterKnownIsUnknown(t *testing.T) {
	api := &fakeSupabaseAPI{}
	r, clock, logs := newTestDataAPI(api)
	api.set(fakeSupabaseResult{schemas: []string{"api"}})
	r.Refresh(context.Background())
	requireSchemas(t, r, []string{"api"})
	clock.advance(time.Hour)
	api.set(fakeSupabaseResult{err: fmt.Errorf("%w: no db_schema", ErrSupabaseMalformed)})
	r.Refresh(context.Background())
	requireSchemas(t, r, nil)
	if logs.matching("no db_schema") != 1 {
		t.Fatalf("malformed response not logged: %q", logs.lines)
	}
	clock.advance(time.Hour)
	api.set(fakeSupabaseResult{schemas: []string{"public"}})
	r.Refresh(context.Background())
	requireSchemas(t, r, []string{"public"})
}

// Invalid input: a SupabaseAPI that answers no schema and no error is
// unknown and logged as malformed, never "no schema served".
func TestSupabaseDataAPI_EmptySchemaListIsUnknown(t *testing.T) {
	api := &fakeSupabaseAPI{}
	r, clock, logs := newTestDataAPI(api)
	api.set(fakeSupabaseResult{schemas: []string{"api"}})
	r.Refresh(context.Background())
	requireSchemas(t, r, []string{"api"})
	clock.advance(time.Hour)
	api.set(fakeSupabaseResult{schemas: []string{}})
	r.Refresh(context.Background())
	requireSchemas(t, r, nil)
	if logs.matching("list no schema") != 1 {
		t.Fatalf("empty schema list not logged as malformed: %q", logs.lines)
	}
}

// A context error is not a provider answer: the read is retried at the
// next refresh and nothing is logged.
func TestSupabaseDataAPI_CancelledReadRetriesAtOnce(t *testing.T) {
	api := &fakeSupabaseAPI{}
	r, _, logs := newTestDataAPI(api)
	api.set(fakeSupabaseResult{err: context.Canceled})
	r.Refresh(context.Background())
	api.set(fakeSupabaseResult{schemas: []string{"public"}})
	r.Refresh(context.Background())
	if api.count() != 1 {
		t.Fatalf("calls after a cancelled read = %d, want a retry", api.count())
	}
	requireSchemas(t, r, []string{"public"})
	if len(logs.lines) != 0 {
		t.Fatalf("cancelled read logged %q", logs.lines)
	}
}

func TestSupabaseDataAPI_ExposureIsACopy(t *testing.T) {
	api := &fakeSupabaseAPI{results: []fakeSupabaseResult{{schemas: []string{"public"}}}}
	r, _, _ := newTestDataAPI(api)
	r.Refresh(context.Background())
	r.Exposure().Schemas[0] = "api"
	requireSchemas(t, r, []string{"public"})
}

func TestSupabaseDataAPI_ConcurrentReadsMakeOneCall(t *testing.T) {
	api := &fakeSupabaseAPI{results: []fakeSupabaseResult{{schemas: []string{"public"}}}}
	r, _, _ := newTestDataAPI(api)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(2)
		go func() { defer wg.Done(); r.Refresh(context.Background()) }()
		go func() { defer wg.Done(); _ = r.Exposure() }()
	}
	wg.Wait()
	if api.count() != 1 {
		t.Fatalf("calls = %d, want 1 within the hour", api.count())
	}
	requireSchemas(t, r, []string{"public"})
}

// Run refreshes at once, then on every tick, until ctx ends.
func TestSupabaseDataAPI_RunRefreshesUntilCancelled(t *testing.T) {
	api := &fakeSupabaseAPI{results: []fakeSupabaseResult{{schemas: []string{"public"}},
		{schemas: []string{"api"}}}}
	r, clock, _ := newTestDataAPI(api)
	ticks := make(chan time.Time)
	r.opts.After = func(time.Duration) <-chan time.Time { return ticks }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for api.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	clock.advance(time.Hour)
	ticks <- time.Time{} // Run waits only after its first refresh
	ticks <- time.Time{} // and again only after the tick's refresh
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if api.count() != 2 {
		t.Fatalf("calls = %d, want 2 (start and one tick after an hour)", api.count())
	}
	requireSchemas(t, r, []string{"api"})
}

func TestSupabaseDataAPI_ErrorsAreDistinct(t *testing.T) {
	all := []error{ErrSupabaseUnauthorized, ErrSupabaseRateLimited, ErrSupabaseMalformed,
		ErrSupabaseAPI}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("%v matches %v", a, b)
			}
		}
	}
}
