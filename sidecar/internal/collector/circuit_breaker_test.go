package collector

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// collection-08: the breaker says why it skipped (load or a failed load
// query) and the collector's Status reports its state.

// loadRow scans a scripted load ratio, or fails with err.
type loadRow struct {
	ratio float64
	err   error
}

func (r loadRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("load row: want one destination")
	}
	p, ok := dest[0].(*float64)
	if !ok {
		return errors.New("load row: destination is not *float64")
	}
	*p = r.ratio
	return nil
}

// fakeLoadDB answers the load query with a scripted ratio or error.
type fakeLoadDB struct {
	mu    sync.Mutex
	ratio float64
	err   error
	calls int
}

func (f *fakeLoadDB) set(ratio float64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ratio, f.err = ratio, err
}

func (f *fakeLoadDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if sql != loadRatioSQL {
		return loadRow{err: errors.New("fake load db: unexpected statement")}
	}
	return loadRow{ratio: f.ratio, err: f.err}
}

func (f *fakeLoadDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fake load db: Query is not scripted")
}

func TestBreakerLoadQueryErrorIsNamed(t *testing.T) {
	cb := NewCircuitBreaker(90, 3)
	d := cb.ShouldSkip(context.Background(),
		&fakeLoadDB{err: context.DeadlineExceeded})
	if !d.Skip {
		t.Fatal("a failed load query must still skip the cycle")
	}
	if d.Reason != SkipReasonLoadQueryError {
		t.Errorf("Reason = %q, want %q", d.Reason, SkipReasonLoadQueryError)
	}
	if !errors.Is(d.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want it to wrap context.DeadlineExceeded", d.Err)
	}
	if d.Ratio != 0 {
		t.Errorf("Ratio = %v, want 0 (unknown) on a failed query", d.Ratio)
	}
}

func TestBreakerOverloadIsNamed(t *testing.T) {
	cb := NewCircuitBreaker(90, 3)
	d := cb.ShouldSkip(context.Background(), &fakeLoadDB{ratio: 0.93})
	if !d.Skip || d.Reason != SkipReasonLoad || d.Err != nil {
		t.Fatalf("decision = %+v, want a load skip without error", d)
	}
	if d.Ratio != 0.93 || d.Ceiling != 0.90 {
		t.Errorf("Ratio/Ceiling = %v/%v, want 0.93/0.90", d.Ratio, d.Ceiling)
	}
}

func TestBreakerCeilingBoundary(t *testing.T) {
	cases := []struct {
		ratio float64
		skip  bool
	}{
		{0, false},
		{0.8999, false},
		{0.90, false}, // at the ceiling: not over it
		{0.9001, true},
		{1.5, true},
	}
	for _, tc := range cases {
		cb := NewCircuitBreaker(90, 3)
		d := cb.ShouldSkip(context.Background(), &fakeLoadDB{ratio: tc.ratio})
		if d.Skip != tc.skip {
			t.Errorf("ratio %v: Skip = %v, want %v", tc.ratio, d.Skip, tc.skip)
		}
		if !tc.skip && (d.Reason != "" || d.Err != nil) {
			t.Errorf("ratio %v: a pass has Reason %q Err %v, want none",
				tc.ratio, d.Reason, d.Err)
		}
		if d.Ratio != tc.ratio {
			t.Errorf("ratio %v: Decision.Ratio = %v", tc.ratio, d.Ratio)
		}
	}
}

// breakerCollector is a collector whose load query is scripted; the
// breaker trips after three skips like the default configuration's.
func breakerCollector(t *testing.T, db *fakeLoadDB) (*Collector, *logRecorder) {
	t.Helper()
	rec := &logRecorder{}
	cfg := testConfig()
	cfg.Safety.BackoffConsecutiveSkips = 3
	c := New(nil, cfg, 170000, rec.log)
	c.loadDB = db
	return c, rec
}

func assertBreakerState(t *testing.T, c *Collector, step string, want BreakerState) {
	t.Helper()
	if got := c.Status().BreakerState; got != want {
		t.Errorf("%s: BreakerState = %q, want %q", step, got, want)
	}
}

func TestBreakerStateTransitionsOnLoadQueryErrors(t *testing.T) {
	db := &fakeLoadDB{err: context.DeadlineExceeded}
	c, _ := breakerCollector(t, db)
	assertBreakerState(t, c, "start", BreakerClosed)
	ticker := &fakeTicker{}
	for i, want := range []BreakerState{BreakerSkipping, BreakerSkipping, BreakerDormant} {
		c.cycle(context.Background(), ticker)
		assertBreakerState(t, c, "error "+string(rune('1'+i)), want)
	}
	st := c.Status()
	if st.ConsecutiveSkips != 3 || st.BreakerReason != SkipReasonLoadQueryError {
		t.Errorf("skips/reason = %d/%q, want 3/%q", st.ConsecutiveSkips,
			st.BreakerReason, SkipReasonLoadQueryError)
	}
	if st.SkipsTotal[SkipReasonLoadQueryError] != 3 || st.SkipsTotal[SkipReasonLoad] != 0 {
		t.Errorf("SkipsTotal = %v, want 3 load_query_error, 0 load", st.SkipsTotal)
	}
	if !strings.Contains(st.LastErrorText, "deadline exceeded") || st.LastErrorAt.IsZero() {
		t.Errorf("last error = %q at %v, want the load query's", st.LastErrorText,
			st.LastErrorAt)
	}
	resets := ticker.got()
	if len(resets) != 1 || resets[0] != c.cfg.Safety.DormantInterval() {
		t.Errorf("ticker resets = %v, want one to the dormant interval", resets)
	}
}

func TestBreakerStateTransitionsOnOverloadAndRecovery(t *testing.T) {
	db := &fakeLoadDB{ratio: 0.95}
	c, _ := breakerCollector(t, db)
	ticker := &fakeTicker{}
	for i, want := range []BreakerState{BreakerSkipping, BreakerSkipping, BreakerDormant} {
		c.cycle(context.Background(), ticker)
		assertBreakerState(t, c, "overload "+string(rune('1'+i)), want)
	}
	st := c.Status()
	if st.LastLoadRatio != 0.95 || st.BreakerReason != SkipReasonLoad {
		t.Errorf("ratio/reason = %v/%q, want 0.95/load", st.LastLoadRatio, st.BreakerReason)
	}
	if !st.LastErrorAt.IsZero() {
		t.Errorf("an overload is not an error; LastErrorAt = %v", st.LastErrorAt)
	}
	// Recovery needs three successful cycles, as before.
	for i, want := range []BreakerState{BreakerDormant, BreakerDormant, BreakerClosed} {
		c.breaker.RecordSuccess()
		assertBreakerState(t, c, "success "+string(rune('1'+i)), want)
	}
	if got := c.Status(); got.ConsecutiveSkips != 0 || got.BreakerReason != "" {
		t.Errorf("after recovery skips/reason = %d/%q, want 0/\"\"",
			got.ConsecutiveSkips, got.BreakerReason)
	}
	if got := c.Status().SkipsTotal[SkipReasonLoad]; got != 3 {
		t.Errorf("SkipsTotal[load] = %d after recovery, want 3 (monotonic)", got)
	}
}

func TestCycleLogsLoadQueryCauseNotLoad(t *testing.T) {
	db := &fakeLoadDB{err: context.DeadlineExceeded}
	c, rec := breakerCollector(t, db)
	for i := 0; i < 3; i++ { // through dormancy
		c.cycle(context.Background(), &fakeTicker{})
	}
	if !rec.has("WARN", "load query failed: context deadline exceeded") {
		t.Errorf("no WARN names the failed load query; logs: %v", rec.lines)
	}
	for _, line := range rec.lines {
		if strings.Contains(line, "db load too high") || strings.Contains(line, "load ratio") {
			t.Errorf("a failed load query was logged as load: %q", line)
		}
	}
}

func TestCycleLogsLoadRatioAndCeiling(t *testing.T) {
	c, rec := breakerCollector(t, &fakeLoadDB{ratio: 0.93})
	c.cycle(context.Background(), &fakeTicker{})
	want := "load ratio 0.93 > ceiling 0.90 (active sessions / max_connections)"
	if !rec.has("WARN", want) {
		t.Errorf("no WARN %q; logs: %v", want, rec.lines)
	}
	if rec.has("WARN", "load query failed") {
		t.Errorf("an overload was logged as a query failure: %v", rec.lines)
	}
}

// A ratio just over the ceiling must not read as "0.90 > ceiling 0.90".
func TestSkipCauseJustOverCeilingShowsDifference(t *testing.T) {
	cases := map[float64]string{
		0.9001:    "load ratio 0.9001 > ceiling 0.90 ",
		0.904:     "load ratio 0.904 > ceiling 0.90 ",
		0.906:     "load ratio 0.91 > ceiling 0.90 ",
		2.0 / 3.0: "load ratio 0.67 > ceiling 0.60 ",
	}
	for ratio, want := range cases {
		ceiling := 0.90
		if ratio < 0.9 {
			ceiling = 0.60
		}
		d := Decision{Skip: true, Reason: SkipReasonLoad, Ratio: ratio, Ceiling: ceiling}
		if got := d.cause(); !strings.HasPrefix(got, want) {
			t.Errorf("ratio %v: cause = %q, want prefix %q", ratio, got, want)
		}
	}
}

func TestCycleLoadErrorThenOverloadReportsLatestReason(t *testing.T) {
	db := &fakeLoadDB{err: errors.New("permission denied for view pg_stat_activity")}
	c, _ := breakerCollector(t, db)
	c.cycle(context.Background(), &fakeTicker{})
	if got := c.Status().BreakerReason; got != SkipReasonLoadQueryError {
		t.Fatalf("reason after error = %q", got)
	}
	db.set(0.99, nil)
	c.cycle(context.Background(), &fakeTicker{})
	st := c.Status()
	if st.BreakerReason != SkipReasonLoad || st.ConsecutiveSkips != 2 {
		t.Errorf("reason/skips = %q/%d, want load/2 (both count toward dormancy)",
			st.BreakerReason, st.ConsecutiveSkips)
	}
	if !strings.Contains(st.LastErrorText, "permission denied") {
		t.Errorf("LastErrorText = %q, want the earlier load query error kept",
			st.LastErrorText)
	}
}
