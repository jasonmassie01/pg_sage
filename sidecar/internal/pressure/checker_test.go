package pressure

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// guard-01a: the checker caches each table's answer for 2 s, folds
// concurrent calls into one read set, measures the WAL rate and the
// replica catch-up rate across read sets, and turns limits and storage
// failures into verdicts. The source is a recorded fake; pressure_db_test
// drives the SQL source on a real server.

// clock is a manual clock; sleep advances it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	c.advance(d)
	return ctx.Err()
}

// recordedSource replays recorded readings: lags[i] is the replica lag of
// read set i (the last repeats), wal[i] the i-th WAL position asked for.
type recordedSource struct {
	clock    *clock
	mu       sync.Mutex
	lags     []float64
	wal      []float64
	walCalls int
	reads    atomic.Int32
	tables   []string
	delay    time.Duration
	err      error
	mutate   func(Readings) Readings
}

func (s *recordedSource) Read(ctx context.Context, table string) (Readings, error) {
	n := int(s.reads.Add(1)) - 1
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tables = append(s.tables, table)
	if s.err != nil {
		return Readings{}, s.err
	}
	now := s.clock.Now()
	lag := NotApplicable("no replicas")
	if len(s.lags) > 0 {
		lag = Known(s.lags[min(n, len(s.lags)-1)], now)
	}
	r := Readings{Inputs: map[string]Reading{
		policy.PressureReplicaLag:   lag,
		policy.PressureXIDAge:       Known(1000, now),
		policy.PressureMultixactAge: Known(1, now),
		policy.PressureVacuum:       Known(0, now),
		policy.PressureLockQueue:    Known(0, now),
	}, WALPosition: s.nextWAL(now), XIDFreezeMax: 200_000_000, MXIDFreezeMax: 400_000_000}
	if s.mutate != nil {
		r = s.mutate(r)
	}
	return r, nil
}

func (s *recordedSource) nextWAL(now time.Time) Reading {
	if len(s.wal) == 0 {
		return Known(0, now)
	}
	v := s.wal[min(s.walCalls, len(s.wal)-1)]
	s.walCalls++
	return Known(v, now)
}

func (s *recordedSource) WALPosition(context.Context) Reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextWAL(s.clock.Now())
}

type fakeStorage struct {
	free, capacity float64
	err            error
}

func (s fakeStorage) FreeStorage(context.Context) (float64, float64, time.Time, error) {
	return s.free, s.capacity, t0, s.err
}

func newTestChecker(src *recordedSource, mutate func(*Options)) *Checker {
	o := Options{Database: "orders", Source: src, Now: src.clock.Now, Sleep: src.clock.sleep,
		Storage: fakeStorage{free: 80, capacity: 100}}
	if mutate != nil {
		mutate(&o)
	}
	return NewChecker(o)
}

func newClock() *clock { return &clock{now: t0} }

func mustCheck(t *testing.T, c *Checker, req Request) Verdict {
	t.Helper()
	v, err := c.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("check %+v: %v", req, err)
	}
	return v
}

func TestPressureChecker_AnswersWithRequestIdentity(t *testing.T) {
	src := &recordedSource{clock: newClock(), lags: []float64{0}}
	v := mustCheck(t, newTestChecker(src, nil), Request{Database: "orders",
		Table: "public.events", Job: "backfill-42", Batch: 500})
	if v.Verdict != Proceed || v.Database != "orders" || v.Table != "public.events" ||
		v.Job != "backfill-42" || v.SuggestedBatch != 500 {
		t.Fatalf("verdict = %+v, want proceed for orders/public.events/backfill-42", v)
	}
	// The first read set also measures the WAL rate over a 500 ms span.
	done := t0.Add(500 * time.Millisecond)
	if !v.CheckedAt.Equal(done) || !v.CachedUntil.Equal(done.Add(2*time.Second)) {
		t.Fatalf("checked %v cached until %v, want t0+500ms and 2 s later", v.CheckedAt,
			v.CachedUntil)
	}
	if got := inputOf(t, v, policy.PressureFreeStorage); got.Value == nil || *got.Value != 80 {
		t.Fatalf("free storage = %+v, want 80%%", got)
	}
}

func TestPressureChecker_CachesEachTableForTwoSeconds(t *testing.T) {
	clk := newClock()
	src := &recordedSource{clock: clk}
	c := newTestChecker(src, nil)
	mustCheck(t, c, Request{Table: "a"})
	clk.advance(1999 * time.Millisecond)
	mustCheck(t, c, Request{Table: "a"})
	if n := src.reads.Load(); n != 1 {
		t.Fatalf("reads within 2 s = %d, want 1", n)
	}
	mustCheck(t, c, Request{Table: "b"})
	if n := src.reads.Load(); n != 2 {
		t.Fatalf("reads after a second table = %d, want 2 (per-table cache)", n)
	}
	clk.advance(time.Millisecond)
	mustCheck(t, c, Request{Table: "a"})
	if n := src.reads.Load(); n != 3 {
		t.Fatalf("reads at exactly 2 s = %d, want a fresh read (3)", n)
	}
}

func TestPressureChecker_HundredConcurrentCallsShareOneReadSet(t *testing.T) {
	src := &recordedSource{clock: newClock(), delay: 50 * time.Millisecond}
	c := newTestChecker(src, nil)
	var wg sync.WaitGroup
	var failures atomic.Int32
	verdicts := make([]Verdict, 100)
	for i := range verdicts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.Check(context.Background(), Request{Table: "public.t"})
			if err != nil {
				failures.Add(1)
			}
			verdicts[i] = v
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 || src.reads.Load() != 1 {
		t.Fatalf("100 concurrent calls: %d failures, %d read sets; want 0 and 1",
			failures.Load(), src.reads.Load())
	}
	for i, v := range verdicts {
		if v.Verdict != Proceed || len(v.Inputs) != len(policy.PressureInputs()) {
			t.Fatalf("caller %d got %+v, want the shared proceed", i, v)
		}
	}
}

func TestPressureChecker_StateTransitionsFollowTheLoad(t *testing.T) {
	clk := newClock()
	src := &recordedSource{clock: clk, lags: []float64{0, 10, 20, 10, 0}}
	c := newTestChecker(src, nil)
	want := []Kind{Proceed, Slow, Stop, Slow, Proceed}
	for i, w := range want {
		if v := mustCheck(t, c, Request{Table: "t"}); v.Verdict != w {
			t.Fatalf("step %d (lag %v) = %s, want %s", i, src.lags[i], v.Verdict, w)
		}
		clk.advance(2 * time.Second)
	}
}

// The recorded replication source rises 1 s of lag per second; the
// verdict stops while the lag is still under the 30 s alert threshold.
func TestPressureChecker_RecordedReplicationStopsBeforeTheAlert(t *testing.T) {
	clk := newClock()
	lags := make([]float64, 31)
	for i := range lags {
		lags[i] = float64(i) * 2
	}
	src := &recordedSource{clock: clk, lags: lags}
	c := newTestChecker(src, func(o *Options) { o.Alerts = Alerts{ReplicaLagSeconds: 30} })
	for i := range lags {
		v := mustCheck(t, c, Request{Table: "t"})
		if v.Verdict == Stop {
			if lags[i] >= 30 {
				t.Fatalf("stopped at lag %v, at or past the 30 s alert", lags[i])
			}
			if lags[i] < 15 {
				t.Fatalf("stopped at lag %v, under the 15 s stop limit", lags[i])
			}
			return
		}
		clk.advance(2 * time.Second)
	}
	t.Fatal("the verdict never stopped while lag rose to 60 s")
}

func TestPressureChecker_SleepUsesTheMeasuredCatchUpRate(t *testing.T) {
	clk := newClock()
	src := &recordedSource{clock: clk, lags: []float64{12, 10}}
	c := newTestChecker(src, nil)
	first := mustCheck(t, c, Request{Table: "t"})
	if first.SleepSeconds != 4.5 {
		t.Fatalf("first sleep = %v, want the unmeasured estimate 12-7.5 = 4.5",
			first.SleepSeconds)
	}
	clk.advance(2 * time.Second)
	second := mustCheck(t, c, Request{Table: "t"})
	// 2 s of lag recovered over 2.5 s (the first read set's 500 ms WAL span
	// plus the 2 s wait): 0.8 s/s, so (10-7.5)/0.8 = 3.125 s.
	if second.Verdict != Slow || math.Abs(second.SleepSeconds-3.125) > 1e-9 {
		t.Fatalf("second = %s sleep %v, want slow and 3.125 s", second.Verdict,
			second.SleepSeconds)
	}
}

func TestPressureChecker_WALRateOverTheWindow(t *testing.T) {
	clk := newClock()
	const mib = 1 << 20
	src := &recordedSource{clock: clk, wal: []float64{0, 1 * mib, 9 * mib, 200 * mib, 201 * mib}}
	c := newTestChecker(src, nil)
	first := mustCheck(t, c, Request{Table: "t"})
	wal := inputOf(t, first, policy.PressureWALRate)
	if wal.Value == nil || *wal.Value != 2*mib {
		t.Fatalf("first WAL rate = %+v, want 1 MiB over the 500 ms first span = 2 MiB/s", wal)
	}
	clk.advance(2 * time.Second)
	second := inputOf(t, mustCheck(t, c, Request{Table: "t"}), policy.PressureWALRate)
	if second.Value == nil || *second.Value != 9*mib/2.5 {
		t.Fatalf("second WAL rate = %v, want 9 MiB over 2.5 s", deref(second.Value))
	}
	clk.advance(70 * time.Second)
	third := inputOf(t, mustCheck(t, c, Request{Table: "t"}), policy.PressureWALRate)
	if third.Value == nil || *third.Value != 2*mib {
		t.Fatalf("after a 70 s gap the rate = %v, want a fresh 500 ms span (2 MiB/s)",
			deref(third.Value))
	}
}

func TestPressureChecker_WALPositionGoingBackResetsTheWindow(t *testing.T) {
	clk := newClock()
	src := &recordedSource{clock: clk, wal: []float64{100, 200, 50, 150}}
	c := newTestChecker(src, nil)
	mustCheck(t, c, Request{Table: "t"})
	clk.advance(2 * time.Second)
	v := inputOf(t, mustCheck(t, c, Request{Table: "t"}), policy.PressureWALRate)
	if v.Value == nil || *v.Value != 200 {
		t.Fatalf("after a failover the rate = %v, want 100 bytes over a fresh 500 ms span",
			deref(v.Value))
	}
}

func TestPressureChecker_UnreadableWALPositionIsUnknown(t *testing.T) {
	src := &recordedSource{clock: newClock(), mutate: func(r Readings) Readings {
		r.WALPosition = Unknown("the server is a standby")
		return r
	}}
	v := mustCheck(t, newTestChecker(src, nil), Request{Table: "t"})
	if in := inputOf(t, v, policy.PressureWALRate); in.State != StateUnknown ||
		!strings.Contains(in.Reason, "standby") {
		t.Fatalf("WAL rate = %+v, want unknown with the position's reason", in)
	}
	if v.Verdict != Proceed {
		t.Fatalf("an unknown optional WAL rate = %s, want proceed", v.Verdict)
	}
}

func TestPressureChecker_TimeoutOnOneInputFollowsThePolicy(t *testing.T) {
	timedOut := func(r Readings) Readings {
		r.Inputs[policy.PressureLockQueue] = Unknown("timed out after 1s")
		return r
	}
	src := &recordedSource{clock: newClock(), mutate: timedOut}
	v := mustCheck(t, newTestChecker(src, nil), Request{Table: "t"})
	if v.Verdict != Stop || inputOf(t, v, policy.PressureLockQueue).Reason !=
		"timed out after 1s" {
		t.Fatalf("required lock queue timed out = %+v, want stop with the reason", v)
	}
	optional := func(context.Context) (Limits, error) {
		l := DefaultLimits(Alerts{})
		delete(l.Required, policy.PressureLockQueue)
		return l, nil
	}
	src2 := &recordedSource{clock: newClock(), mutate: timedOut}
	v2 := mustCheck(t, newTestChecker(src2, func(o *Options) { o.Limits = optional }),
		Request{Table: "t"})
	if v2.Verdict != Proceed || inputOf(t, v2, policy.PressureLockQueue).State != StateUnknown {
		t.Fatalf("optional lock queue timed out = %s, want proceed reporting it unknown",
			v2.Verdict)
	}
}

func TestPressureChecker_UnreadableLimitsStop(t *testing.T) {
	src := &recordedSource{clock: newClock()}
	broken := func(context.Context) (Limits, error) {
		return Limits{}, errors.New("read the standing policy: connection refused")
	}
	v := mustCheck(t, newTestChecker(src, func(o *Options) { o.Limits = broken }),
		Request{Table: "t"})
	if v.Verdict != Stop || !strings.Contains(strings.Join(v.Reasons, ";"),
		"connection refused") {
		t.Fatalf("unreadable limits = %s %v, want stop naming the failure", v.Verdict, v.Reasons)
	}
	if len(v.Inputs) != len(policy.PressureInputs()) {
		t.Fatalf("inputs = %d, want every input still reported", len(v.Inputs))
	}
}

func TestPressureChecker_StorageSources(t *testing.T) {
	cases := []struct {
		name    string
		storage StorageSource
		state   State
		reason  string
		want    Kind
	}{
		{"no adapter", nil, StateUnknown, "no cloud storage adapter", Proceed},
		{"adapter failure", fakeStorage{err: errors.New("no fresh storage sample")},
			StateUnknown, "no fresh storage sample", Proceed},
		{"zero capacity", fakeStorage{free: 1, capacity: 0}, StateUnknown, "capacity", Proceed},
		{"nearly full", fakeStorage{free: 15, capacity: 100}, StateKnown, "", Stop},
	}
	for _, c := range cases {
		src := &recordedSource{clock: newClock()}
		v := mustCheck(t, newTestChecker(src, func(o *Options) { o.Storage = c.storage }),
			Request{Table: "t"})
		in := inputOf(t, v, policy.PressureFreeStorage)
		if in.State != c.state || !strings.Contains(in.Reason, c.reason) || v.Verdict != c.want {
			t.Errorf("%s: storage %+v verdict %s, want %s %q %s", c.name, in, v.Verdict,
				c.state, c.reason, c.want)
		}
	}
}

func TestPressureChecker_SourceErrorsPropagate(t *testing.T) {
	src := &recordedSource{clock: newClock(), err: ErrUnknownTable}
	c := newTestChecker(src, nil)
	if _, err := c.Check(context.Background(), Request{Table: "nope"}); !errors.Is(err,
		ErrUnknownTable) {
		t.Fatalf("unknown table err = %v, want ErrUnknownTable", err)
	}
	src2 := &recordedSource{clock: newClock(), err: errors.New("dial tcp: connection refused")}
	_, err := newTestChecker(src2, nil).Check(context.Background(), Request{Table: "t"})
	if err == nil || !strings.Contains(err.Error(), "orders") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("source failure err = %v, want it to name the database and the cause", err)
	}
}

func TestPressureChecker_RefusesInvalidRequestsWithoutReading(t *testing.T) {
	src := &recordedSource{clock: newClock()}
	c := newTestChecker(src, nil)
	for _, req := range []Request{
		{Table: "a\x00b"}, {Table: strings.Repeat("x", 300)},
		{Table: "t", Job: strings.Repeat("j", 129)}, {Table: "t", Job: "bad\njob"},
		{Table: "t", Batch: -1},
	} {
		if _, err := c.Check(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("request %q/%q/%d err = %v, want ErrInvalidRequest", req.Table, req.Job,
				req.Batch, err)
		}
	}
	if n := src.reads.Load(); n != 0 {
		t.Fatalf("invalid requests read %d times, want 0", n)
	}
}

func TestPressureChecker_NoTableMakesTableInputsNotApplicable(t *testing.T) {
	src := &recordedSource{clock: newClock()}
	v := mustCheck(t, newTestChecker(src, nil), Request{})
	if src.tables[0] != "" {
		t.Fatalf("source asked for table %q, want none", src.tables[0])
	}
	if v.Verdict != Proceed {
		t.Fatalf("database-only check = %s, want proceed", v.Verdict)
	}
}

func TestPressureChecker_NilAndUnwiredAreUnavailable(t *testing.T) {
	var nilChecker *Checker
	if _, err := nilChecker.Check(context.Background(), Request{}); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("nil checker err = %v, want ErrUnavailable", err)
	}
	if _, err := NewChecker(Options{}).Check(context.Background(), Request{}); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("checker without a source err = %v, want ErrUnavailable", err)
	}
}

func TestPressureChecker_CallerCancellationDoesNotPoisonTheCache(t *testing.T) {
	src := &recordedSource{clock: newClock()}
	c := newTestChecker(src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, Request{Table: "t"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller err = %v, want context.Canceled", err)
	}
	if v := mustCheck(t, c, Request{Table: "t"}); v.Verdict != Proceed {
		t.Fatalf("next caller = %s, want a real answer", v.Verdict)
	}
}

// pg_stat_replication's replay_lag only moves when the standby replies, so
// with replay paused it can read a second while the standby is far behind.
// The WAL position history bounds the lag from below: the primary was past
// the replica's replay position at the oldest sample beyond it.
func TestPressureChecker_ReplayPositionOutrunsAStaleReplayLag(t *testing.T) {
	clk := newClock()
	wal := make([]float64, 40)
	for i := range wal {
		wal[i] = float64(i+1) * 1000
	}
	paused := func(r Readings) Readings {
		r.ReplayPosition = Known(500, clk.Now()) // replayed nothing past t0
		return r
	}
	src := &recordedSource{clock: clk, lags: []float64{1}, wal: wal, mutate: paused}
	c := newTestChecker(src, nil)
	for i := 0; i < 20; i++ {
		v := mustCheck(t, c, Request{Table: "t"})
		lag := inputOf(t, v, policy.PressureReplicaLag)
		// The first sample past 500 bytes is t0 (1000 bytes).
		want := clk.Now().Sub(t0).Seconds()
		if lag.Value == nil || math.Abs(*lag.Value-max(1, want)) > 1e-9 {
			t.Fatalf("call %d: lag %v, want max(replay_lag 1, %v)", i, deref(lag.Value), want)
		}
		if v.Verdict == Stop {
			if *lag.Value < 15 || *lag.Value >= 30 {
				t.Fatalf("stopped at %v s, want between the 15 s stop and the 30 s alert",
					*lag.Value)
			}
			if !strings.Contains(lag.Reason, "WAL position") {
				t.Fatalf("lag reason %q does not say it was estimated", lag.Reason)
			}
			return
		}
		clk.advance(2 * time.Second)
	}
	t.Fatal("a paused replica never stopped the answer")
}

func TestPressureChecker_CaughtUpReplicaKeepsTheReportedLag(t *testing.T) {
	clk := newClock()
	src := &recordedSource{clock: clk, lags: []float64{3}, wal: []float64{0, 100, 200},
		mutate: func(r Readings) Readings {
			r.ReplayPosition = NotApplicable("no replica behind")
			return r
		}}
	c := newTestChecker(src, nil)
	mustCheck(t, c, Request{Table: "t"})
	clk.advance(20 * time.Second)
	lag := inputOf(t, mustCheck(t, c, Request{Table: "t"}), policy.PressureReplicaLag)
	if lag.Value == nil || *lag.Value != 3 || lag.Reason != "" {
		t.Fatalf("caught-up lag = %+v, want the reported 3 s unchanged", lag)
	}
}
