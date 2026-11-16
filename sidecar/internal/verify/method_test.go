package verify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lab-01c: verify.method auto acts on v2 only on a database whose latest
// back-test passed and is at most 35 days old; otherwise it is shadow (v2
// computed beside v1, v1 acted on). The back-test is read through a
// 10-minute cache, so a new back-test switches the method without a
// restart.

type fakeBacktests struct {
	mu    sync.Mutex
	rows  map[string]Backtest
	err   error
	reads atomic.Int64
}

func (f *fakeBacktests) LatestBacktest(_ context.Context, database string) (Backtest, bool,
	error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Backtest{}, false, f.err
	}
	b, ok := f.rows[database]
	return b, ok, nil
}

func (f *fakeBacktests) set(database string, b Backtest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]Backtest{}
	}
	f.rows[database] = b
}

var methodNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func resolve(t *testing.T, r *MethodResolver, configured string) Resolution {
	t.Helper()
	got, err := r.Resolve(context.Background(), configured, "orders")
	if err != nil {
		t.Fatalf("Resolve(%s): %v", configured, err)
	}
	return got
}

func TestVerifyMethod_ExplicitMethodsPassThrough(t *testing.T) {
	reader := &fakeBacktests{}
	r := NewMethodResolver(reader, func() time.Time { return methodNow })
	for _, m := range []string{MethodV1, MethodShadow, MethodV2} {
		if got := resolve(t, r, m); got.Method != m {
			t.Fatalf("configured %s resolved to %+v", m, got)
		}
	}
	if reader.reads.Load() != 0 {
		t.Fatalf("an explicit method read the back-test %d times", reader.reads.Load())
	}
}

func TestVerifyMethod_AutoWithoutBacktestIsShadow(t *testing.T) {
	r := NewMethodResolver(NoBacktest{}, func() time.Time { return methodNow })
	got := resolve(t, r, MethodAuto)
	if got.Method != MethodShadow || !strings.Contains(got.Reason, "no back-test") {
		t.Fatalf("auto without a back-test: %+v, want shadow naming the missing back-test",
			got)
	}
}

func TestVerifyMethod_BacktestAgeBoundary(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{35 * 24 * time.Hour, MethodV2},
		{35*24*time.Hour + time.Second, MethodShadow},
		{36 * 24 * time.Hour, MethodShadow},
		{0, MethodV2},
	} {
		reader := &fakeBacktests{}
		reader.set("orders", Backtest{Passed: true, RanAt: methodNow.Add(-tc.age)})
		r := NewMethodResolver(reader, func() time.Time { return methodNow })
		if got := resolve(t, r, MethodAuto); got.Method != tc.want {
			t.Fatalf("back-test %s old: %+v, want %s", tc.age, got, tc.want)
		}
	}
}

func TestVerifyMethod_FailedOrFutureBacktestIsShadow(t *testing.T) {
	for name, b := range map[string]Backtest{
		"failed": {Passed: false, RanAt: methodNow.Add(-time.Hour)},
		"future": {Passed: true, RanAt: methodNow.Add(time.Hour)},
		"zero":   {Passed: true},
	} {
		reader := &fakeBacktests{}
		reader.set("orders", b)
		r := NewMethodResolver(reader, func() time.Time { return methodNow })
		if got := resolve(t, r, MethodAuto); got.Method != MethodShadow || got.Reason == "" {
			t.Fatalf("%s back-test: %+v, want shadow with a reason", name, got)
		}
	}
}

// A back-test that cannot be read fails closed to shadow (v1 acts), the
// cause is returned, and the failure is not cached.
func TestVerifyMethod_ReadErrorIsShadowAndNotCached(t *testing.T) {
	reader := &fakeBacktests{err: errors.New("relation does not exist")}
	r := NewMethodResolver(reader, func() time.Time { return methodNow })
	got, err := r.Resolve(context.Background(), MethodAuto, "orders")
	if err == nil || !strings.Contains(err.Error(), "relation does not exist") ||
		!strings.Contains(err.Error(), "orders") {
		t.Fatalf("err %v, want the cause and the database", err)
	}
	if got.Method != MethodShadow {
		t.Fatalf("read error resolved to %+v, want shadow", got)
	}
	_, _ = r.Resolve(context.Background(), MethodAuto, "orders")
	if reader.reads.Load() != 2 {
		t.Fatalf("reads %d, want 2: a failed read must not be cached", reader.reads.Load())
	}
}

func TestVerifyMethod_UnknownMethodIsRefusedAsV1(t *testing.T) {
	r := NewMethodResolver(NoBacktest{}, func() time.Time { return methodNow })
	for _, m := range []string{"v3", "", "AUTO"} {
		got, err := r.Resolve(context.Background(), m, "orders")
		if err == nil || !strings.Contains(err.Error(), "verify.method") {
			t.Fatalf("method %q: err %v, want a refusal naming verify.method", m, err)
		}
		if got.Method != MethodV1 {
			t.Fatalf("method %q resolved to %+v, want v1 (today's verifier)", m, got)
		}
	}
}

// State transitions without a restart: shadow becomes v2 once a passing
// back-test appears and the cache entry ages out, and v2 goes back to
// shadow once that back-test is older than 35 days.
func TestVerifyMethod_ShadowToV2AndBackThroughTheCache(t *testing.T) {
	now := methodNow
	reader := &fakeBacktests{}
	r := NewMethodResolver(reader, func() time.Time { return now })
	if got := resolve(t, r, MethodAuto); got.Method != MethodShadow {
		t.Fatalf("start: %+v, want shadow", got)
	}
	reader.set("orders", Backtest{Passed: true, RanAt: now})
	now = now.Add(MethodCacheTTL - time.Second)
	if got := resolve(t, r, MethodAuto); got.Method != MethodShadow {
		t.Fatalf("inside the cache TTL: %+v, want the cached shadow", got)
	}
	now = now.Add(time.Second)
	if got := resolve(t, r, MethodAuto); got.Method != MethodV2 {
		t.Fatalf("after the TTL: %+v, want v2", got)
	}
	now = now.Add(BacktestMaxAge)
	if got := resolve(t, r, MethodAuto); got.Method != MethodShadow {
		t.Fatalf("back-test aged out: %+v, want shadow", got)
	}
	if reader.reads.Load() != 3 {
		t.Fatalf("reads %d, want 3 (one per TTL)", reader.reads.Load())
	}
}

func TestVerifyMethod_CachesPerDatabase(t *testing.T) {
	reader := &fakeBacktests{}
	reader.set("orders", Backtest{Passed: true, RanAt: methodNow})
	r := NewMethodResolver(reader, func() time.Time { return methodNow })
	a, _ := r.Resolve(context.Background(), MethodAuto, "orders")
	b, _ := r.Resolve(context.Background(), MethodAuto, "billing")
	if a.Method != MethodV2 || b.Method != MethodShadow {
		t.Fatalf("orders %+v billing %+v, want v2 and shadow", a, b)
	}
}

func TestVerifyMethod_ConcurrentResolvesAgree(t *testing.T) {
	reader := &fakeBacktests{}
	reader.set("orders", Backtest{Passed: true, RanAt: methodNow})
	r := NewMethodResolver(reader, func() time.Time { return methodNow })
	var wg sync.WaitGroup
	results := make([]string, 32)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := r.Resolve(context.Background(), MethodAuto, "orders")
			if err == nil {
				results[i] = got.Method
			}
		}(i)
	}
	wg.Wait()
	for i, m := range results {
		if m != MethodV2 {
			t.Fatalf("goroutine %d resolved %q, want v2", i, m)
		}
	}
}

func TestVerifyMethod_NilResolverIsShadow(t *testing.T) {
	var r *MethodResolver
	got, err := r.Resolve(context.Background(), MethodAuto, "orders")
	if err != nil || got.Method != MethodShadow {
		t.Fatalf("nil resolver: %+v %v, want shadow", got, err)
	}
}
