package fleetlearn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeFindings stands in for the per-database read. It counts calls and
// the reads in flight, and can delay, fail, block or truncate a source.
type fakeFindings struct {
	mu        sync.Mutex
	calls     map[string]int
	delay     time.Duration
	block     map[string]bool // until the read's context ends
	hang      chan struct{}   // sources in block wait here instead when set
	fail      map[string]error
	truncated map[string]bool
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func newFakeFindings() *fakeFindings {
	return &fakeFindings{calls: map[string]int{}, block: map[string]bool{},
		fail: map[string]error{}, truncated: map[string]bool{}}
}

func (f *fakeFindings) read(ctx context.Context, s Source, _ int) ([]FindingRow, bool,
	error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for m := f.maxFlight.Load(); n > m; m = f.maxFlight.Load() {
		if f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.calls[s.Name]++
	block, err, trunc, hang := f.block[s.Name], f.fail[s.Name], f.truncated[s.Name], f.hang
	f.mu.Unlock()
	if block && hang != nil {
		<-hang
		return nil, false, errors.New("hung source released")
	}
	if block {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	if err != nil {
		return nil, false, err
	}
	return []FindingRow{{Database: s.Name, ID: 1, Category: "missing_index",
		ObjectIdentifier: "public.t|btree(a)", Severity: "warning", Title: "t"}}, trunc, nil
}

func (f *fakeFindings) callsTo(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testReader(f *fakeFindings) (*FindingsReader, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := NewFindingsReader()
	r.read = f.read
	r.now = clk.now
	return r, clk
}

func sourcesNamed(names ...string) []Source {
	out := make([]Source, 0, len(names))
	for _, n := range names {
		out = append(out, Source{Name: n})
	}
	return out
}

func manySources(n int) []Source {
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("db%02d", i))
	}
	return sourcesNamed(names...)
}

func TestFindingsReader_DefaultsMatchThePlan(t *testing.T) {
	r := NewFindingsReader()
	if r.rowTTL != 60*time.Second || r.errTTL != 15*time.Second {
		t.Fatalf("ttl rows=%s errors=%s, want 60s/15s", r.rowTTL, r.errTTL)
	}
	if r.deadline != 15*time.Second || r.readTimeout != 10*time.Second {
		t.Fatalf("deadline=%s read timeout=%s, want 15s/10s", r.deadline, r.readTimeout)
	}
	if cap(r.sem) != 8 || r.read == nil || r.now == nil {
		t.Fatalf("parallel=%d read set=%v now set=%v, want 8 and both set",
			cap(r.sem), r.read != nil, r.now != nil)
	}
}

func TestFindingsReader_CachedWithinTTLAndReReadAfter(t *testing.T) {
	f := newFakeFindings()
	r, clk := testReader(f)
	srcs := sourcesNamed("a", "b", "c")
	first := r.Collect(context.Background(), srcs, 3, 500)
	if first.DatabasesScanned != 3 || len(first.Findings) != 1 ||
		first.Findings[0].Databases != 3 {
		t.Fatalf("first = %+v, want one finding on 3 databases", first)
	}
	clk.add(60*time.Second - time.Millisecond)
	second := r.Collect(context.Background(), srcs, 3, 500)
	for _, s := range srcs {
		if got := f.callsTo(s.Name); got != 1 {
			t.Fatalf("%s read %d times within the TTL, want 1", s.Name, got)
		}
	}
	if second.DatabasesScanned != 3 || len(second.Findings) != 1 {
		t.Fatalf("cached call = %+v, want the same finding", second)
	}
	clk.add(time.Millisecond)
	r.Collect(context.Background(), srcs, 3, 500)
	for _, s := range srcs {
		if got := f.callsTo(s.Name); got != 2 {
			t.Fatalf("%s read %d times at the TTL, want a re-read (2)", s.Name, got)
		}
	}
}

func TestFindingsReader_ErrorsAreCachedForLessTime(t *testing.T) {
	f := newFakeFindings()
	f.fail["bad"] = errors.New("connection refused")
	r, clk := testReader(f)
	srcs := sourcesNamed("bad", "good")
	res := r.Collect(context.Background(), srcs, 2, 500)
	if len(res.Errors) != 1 || res.Errors[0].Database != "bad" ||
		res.Errors[0].Error != "read open findings failed" ||
		!strings.Contains(res.Errors[0].Cause.Error(), "connection refused") {
		t.Fatalf("errors = %+v, want bad with its cause", res.Errors)
	}
	if res.DatabasesScanned != 1 {
		t.Fatalf("scanned = %d, one bad database must not hide the other", res.DatabasesScanned)
	}
	clk.add(15*time.Second - time.Millisecond)
	if again := r.Collect(context.Background(), srcs, 2, 500); len(again.Errors) != 1 ||
		f.callsTo("bad") != 1 {
		t.Fatalf("cached error: errors=%+v reads=%d, want 1/1", again.Errors, f.callsTo("bad"))
	}
	f.mu.Lock()
	delete(f.fail, "bad")
	f.mu.Unlock()
	clk.add(time.Millisecond)
	healed := r.Collect(context.Background(), srcs, 2, 500)
	if f.callsTo("bad") != 2 || len(healed.Errors) != 0 || healed.DatabasesScanned != 2 {
		t.Fatalf("after 15s: reads=%d result=%+v, want a re-read that succeeds",
			f.callsTo("bad"), healed)
	}
	if f.callsTo("good") != 1 {
		t.Fatalf("good re-read after %d reads; rows live 60s", f.callsTo("good"))
	}
	if len(healed.Findings) != 1 || healed.Findings[0].Databases != 2 {
		t.Fatalf("healed findings = %+v, want one on both", healed.Findings)
	}
}

func TestFindingsReader_DeadlineReportsPendingAsTimedOut(t *testing.T) {
	f := newFakeFindings()
	f.block["hang"] = true
	r, _ := testReader(f)
	r.deadline = 300 * time.Millisecond
	r.readTimeout = 5 * time.Second
	start := time.Now()
	res := r.Collect(context.Background(), sourcesNamed("a", "hang", "z"), 2, 500)
	took := time.Since(start)
	if took < r.deadline || took > r.deadline+100*time.Millisecond {
		t.Fatalf("returned after %s, want within %s + 100ms", took, r.deadline)
	}
	if len(res.Errors) != 1 || res.Errors[0].Database != "hang" ||
		res.Errors[0].Error != "timed out" || res.Errors[0].Cause == nil {
		t.Fatalf("errors = %+v, want hang as timed out with a cause", res.Errors)
	}
	if res.DatabasesScanned != 2 || len(res.Findings) != 1 ||
		res.Findings[0].Databases != 2 {
		t.Fatalf("result = %+v, want a and z read and grouped", res)
	}
}

func TestFindingsReader_DefaultDeadlineBoundsAHungSource(t *testing.T) {
	t.Parallel()
	f := newFakeFindings()
	f.block["hang"] = true
	f.hang = make(chan struct{})
	t.Cleanup(func() { close(f.hang) })
	r, _ := testReader(f)
	start := time.Now()
	res := r.Collect(context.Background(), sourcesNamed("a", "hang", "z"), 2, 500)
	if took := time.Since(start); took > 15*time.Second+100*time.Millisecond {
		t.Fatalf("returned after %s, want within 15s + 100ms", took)
	}
	if len(res.Errors) != 1 || res.Errors[0].Database != "hang" ||
		res.Errors[0].Error != "timed out" || res.DatabasesScanned != 2 {
		t.Fatalf("result = %+v, want hang timed out and a, z read", res)
	}
}

func TestFindingsReader_BoundsReadsInFlight(t *testing.T) {
	f := newFakeFindings()
	f.delay = 20 * time.Millisecond
	r, _ := testReader(f)
	start := time.Now()
	res := r.Collect(context.Background(), manySources(50), 2, 500)
	took := time.Since(start)
	if got := f.maxFlight.Load(); got > 8 || got < 2 {
		t.Fatalf("max reads in flight = %d, want parallel but at most 8", got)
	}
	if res.DatabasesScanned != 50 || len(res.Errors) != 0 {
		t.Fatalf("scanned=%d errors=%+v, want all 50", res.DatabasesScanned, res.Errors)
	}
	if took >= 50*f.delay {
		t.Fatalf("50 reads of %s took %s: the fan-out is sequential", f.delay, took)
	}
}

func TestFindingsReader_CoalescesConcurrentCallers(t *testing.T) {
	f := newFakeFindings()
	f.delay = 20 * time.Millisecond
	r, _ := testReader(f)
	srcs := manySources(50)
	var wg sync.WaitGroup
	results := make([]FleetFindingsResult, 10)
	gate := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			results[i] = r.Collect(context.Background(), srcs, 2, 500)
		}(i)
	}
	close(gate)
	wg.Wait()
	for _, s := range srcs {
		if got := f.callsTo(s.Name); got != 1 {
			t.Fatalf("%s read %d times by 10 concurrent callers, want 1", s.Name, got)
		}
	}
	for i, res := range results {
		if res.DatabasesScanned != 50 || len(res.Findings) != 1 ||
			res.Findings[0].Databases != 50 {
			t.Fatalf("caller %d = scanned %d findings %d", i, res.DatabasesScanned,
				len(res.Findings))
		}
	}
	if got := f.maxFlight.Load(); got > 8 {
		t.Fatalf("max reads in flight across callers = %d, want at most 8", got)
	}
}

func TestFindingsReader_PermittedSubsetNeverSeesOthers(t *testing.T) {
	f := newFakeFindings()
	f.truncated["b"] = true
	f.fail["c"] = errors.New("permission denied")
	r, _ := testReader(f)
	all := r.Collect(context.Background(), sourcesNamed("a", "b", "c"), 2, 500)
	if len(all.Findings) != 1 || len(all.Truncated) != 1 || len(all.Errors) != 1 {
		t.Fatalf("all = %+v, want b cached and truncated, c failed", all)
	}
	only := r.Collect(context.Background(), sourcesNamed("a"), 2, 500)
	if len(only.Findings) != 0 || only.DatabasesScanned != 1 {
		t.Fatalf("limited to a = %+v, must not group with cached b", only)
	}
	if len(only.Truncated) != 0 || len(only.Errors) != 0 {
		t.Fatalf("limited to a: truncated=%v errors=%+v, must not name b or c",
			only.Truncated, only.Errors)
	}
	if f.callsTo("a") != 1 {
		t.Fatalf("a read %d times, want the cached read", f.callsTo("a"))
	}
}

func TestFindingsReader_ListsTruncatedDatabases(t *testing.T) {
	f := newFakeFindings()
	f.truncated["big"] = true
	r, _ := testReader(f)
	res := r.Collect(context.Background(), sourcesNamed("big", "small"), 2, 500)
	if len(res.Truncated) != 1 || res.Truncated[0] != "big" {
		t.Fatalf("truncated = %v, want [big]", res.Truncated)
	}
	none := r.Collect(context.Background(), sourcesNamed("small"), 2, 500)
	if none.Truncated == nil || len(none.Truncated) != 0 {
		t.Fatalf("truncated = %#v, want an empty list", none.Truncated)
	}
}

func TestFindingsReader_EmptyAndNilPool(t *testing.T) {
	r := NewFindingsReader()
	empty := r.Collect(context.Background(), nil, 0, 0)
	if empty.Findings == nil || empty.Errors == nil || empty.Truncated == nil {
		t.Fatalf("no sources = %#v, want empty lists, not nil", empty)
	}
	if empty.MinDatabases != MinFleetDatabases || empty.DatabasesScanned != 0 {
		t.Fatalf("no sources = %+v, want minimum 2 and nothing scanned", empty)
	}
	res := r.Collect(context.Background(), sourcesNamed("nopool"), 2, 500)
	if len(res.Errors) != 1 || res.Errors[0].Database != "nopool" ||
		!strings.Contains(res.Errors[0].Cause.Error(), "no connection") {
		t.Fatalf("nil pool = %+v, want an error naming the missing connection", res.Errors)
	}
}

func TestFindingsReader_NewPoolOrLimitReReads(t *testing.T) {
	f := newFakeFindings()
	r, _ := testReader(f)
	p1, p2 := new(pgxpool.Pool), new(pgxpool.Pool)
	r.Collect(context.Background(), []Source{{Name: "a", Pool: p1}}, 2, 500)
	r.Collect(context.Background(), []Source{{Name: "a", Pool: p1}}, 2, 500)
	if f.callsTo("a") != 1 {
		t.Fatalf("same pool read %d times, want 1", f.callsTo("a"))
	}
	r.Collect(context.Background(), []Source{{Name: "a", Pool: p2}}, 2, 500)
	if f.callsTo("a") != 2 {
		t.Fatalf("re-registered database read %d times, want a fresh read", f.callsTo("a"))
	}
	r.Collect(context.Background(), []Source{{Name: "a", Pool: p2}}, 2, 100)
	if f.callsTo("a") != 3 {
		t.Fatalf("new limit read %d times, want a fresh read", f.callsTo("a"))
	}
}

func TestFindingsReader_CallerCancelDoesNotPoisonTheCache(t *testing.T) {
	f := newFakeFindings()
	f.delay = 50 * time.Millisecond
	r, _ := testReader(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := r.Collect(ctx, sourcesNamed("a"), 2, 500)
	if len(gone.Errors) != 1 || gone.Errors[0].Error != "timed out" {
		t.Fatalf("cancelled caller = %+v, want a pending read reported", gone)
	}
	later := r.Collect(context.Background(), sourcesNamed("a"), 2, 500)
	if later.DatabasesScanned != 1 || len(later.Errors) != 0 || f.callsTo("a") != 1 {
		t.Fatalf("later = %+v reads=%d, want the first read finished and reused",
			later, f.callsTo("a"))
	}
}

func TestFindingsReader_EvictsStaleSourcesNoLongerAsked(t *testing.T) {
	f := newFakeFindings()
	r, clk := testReader(f)
	r.Collect(context.Background(), sourcesNamed("gone", "kept"), 2, 500)
	clk.add(61 * time.Second)
	r.Collect(context.Background(), sourcesNamed("kept"), 2, 500)
	r.mu.Lock()
	_, stale := r.cache["gone"]
	n := len(r.cache)
	r.mu.Unlock()
	if stale || n != 1 {
		t.Fatalf("cache holds %d entries (gone kept=%v), want only kept", n, stale)
	}
}

// BenchmarkFleetFindings measures CHECK-14: 40 sources, 3 of them hung.
// "first" is a cold call whose hung sources never answer (the overall
// deadline bounds it); "cached" is a call after the hung reads have ended
// in errors, within their TTL.
func BenchmarkFleetFindings(b *testing.B) {
	srcs := manySources(40)
	hung := func(f *fakeFindings) {
		for _, s := range srcs[:3] {
			f.block[s.Name] = true
		}
	}
	b.Run("first", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f := newFakeFindings()
			hung(f)
			f.hang = make(chan struct{})
			r, _ := testReader(f)
			start := time.Now()
			r.Collect(context.Background(), srcs, 2, 500)
			b.ReportMetric(float64(time.Since(start).Milliseconds()), "first-ms")
			close(f.hang)
		}
	})
	f := newFakeFindings()
	hung(f)
	r, _ := testReader(f)
	r.Collect(context.Background(), srcs, 2, 500) // prime once: hung reads end at 10s
	b.Run("cached", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			r.Collect(context.Background(), srcs, 2, 500)
		}
	})
}
