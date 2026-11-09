package querystore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// requireDB connects to SAGE_DATABASE_URL, skipping if unset/unreachable.
func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_DATABASE_URL not set; skipping query_store integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("cannot connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("cannot ping: %v", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// recordNow writes every sample, changed or not, at the server's now(): a
// fresh Recorder writes its whole first cycle.
func recordNow(ctx context.Context, db Execer, samples []Sample) error {
	_, err := NewRecorder().Record(ctx, db, samples, time.Now())
	return err
}

// window is a query's QueryIntervals answer summed over its buckets.
type window struct {
	intervals int
	calls     int64
	totalMs   float64
	broken    bool
}

// measured reports the window's per-call latency when it is measurable:
// no epoch break and new calls.
func (w window) measured() (float64, bool) {
	if w.broken || w.calls <= 0 {
		return 0, false
	}
	return w.totalMs / float64(w.calls), true
}

// windowOf reads qid's intervals in [from, to] in day buckets.
func windowOf(t *testing.T, ctx context.Context, db any, qid int64,
	from, to time.Time) window {
	t.Helper()
	ivs, err := QueryIntervals(ctx, db, qid, from, to, 24*time.Hour)
	if err != nil {
		t.Fatalf("QueryIntervals(%d): %v", qid, err)
	}
	w := window{intervals: len(ivs)}
	for _, iv := range ivs {
		w.calls += iv.Calls
		w.totalMs += iv.TotalMs
		w.broken = w.broken || iv.EpochBreak
	}
	return w
}

// TestRecordAndQueryIntervals_RoundTrip exercises the DB path: record two
// samples for a queryid and read the interval between them.
func TestRecordAndQueryIntervals_RoundTrip(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()

	const qid = int64(424242424242)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)

	// First sample: 100 calls, 1000ms total.
	if err := recordNow(ctx, pool, []Sample{
		{QueryID: qid, Calls: 100, TotalExecMs: 1000, MeanExecMs: 10, Rows: 100},
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	time.Sleep(15 * time.Millisecond)

	// Second sample: +100 calls, +3000ms => 30ms/call in the window.
	if err := recordNow(ctx, pool, []Sample{
		{QueryID: qid, Calls: 200, TotalExecMs: 4000, MeanExecMs: 20, Rows: 200},
	}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	w := windowOf(t, ctx, pool, qid, now.Add(-time.Hour), now)
	ms, ok := w.measured()
	if !ok || w.calls != 100 || ms < 29.999 || ms > 30.001 {
		t.Fatalf("window = %+v (%.3f ms, ok=%v), want 100 calls at 30 ms", w, ms, ok)
	}
}

// TestRecord_EmptyAndMissing covers the no-op and no-data branches.
func TestRecord_EmptyAndMissing(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()

	n, err := NewRecorder().Record(ctx, pool, nil, time.Now())
	if err != nil || n != 0 {
		t.Errorf("empty Record should no-op: n=%d err=%v", n, err)
	}
	// A queryid with no samples has no intervals.
	w := windowOf(t, ctx, pool, int64(-99887766), time.Now().Add(-time.Hour), time.Now())
	if w != (window{}) {
		t.Errorf("missing queryid window = %+v, want empty", w)
	}
}
