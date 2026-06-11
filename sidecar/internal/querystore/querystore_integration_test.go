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

// TestRecordAndWindowedLatency_RoundTrip exercises the DB path: record two
// samples for a queryid and compute the windowed latency between them.
func TestRecordAndWindowedLatency_RoundTrip(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()

	const qid = int64(424242424242)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)

	// First sample: 100 calls, 1000ms total.
	if err := Record(ctx, pool, []Sample{
		{QueryID: qid, Calls: 100, TotalExecMs: 1000, MeanExecMs: 10, Rows: 100},
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	base := time.Now()
	time.Sleep(15 * time.Millisecond)

	// Second sample: +100 calls, +3000ms => 30ms/call in the window.
	if err := Record(ctx, pool, []Sample{
		{QueryID: qid, Calls: 200, TotalExecMs: 4000, MeanExecMs: 20, Rows: 200},
	}); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	ms, ok, err := WindowedLatencyMs(ctx, pool, qid, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("windowed: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true with two samples")
	}
	if ms < 29 || ms > 31 {
		t.Errorf("windowed latency = %.2f, want ~30ms", ms)
	}

	// Between-windows variant + prune.
	if _, _, err := WindowedLatencyMsBetween(ctx, pool, qid, base.Add(-time.Hour), time.Now()); err != nil {
		t.Errorf("between: %v", err)
	}
	if _, err := Prune(ctx, pool, time.Now().Add(time.Hour)); err != nil {
		t.Errorf("prune: %v", err)
	}
}

// TestRecord_EmptyAndMissing covers the no-op and no-data branches.
func TestRecord_EmptyAndMissing(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()

	if err := Record(ctx, pool, nil); err != nil {
		t.Errorf("empty Record should no-op: %v", err)
	}
	// Windowed latency for a queryid with no samples -> ok=false.
	_, ok, err := WindowedLatencyMs(ctx, pool, int64(-99887766), time.Now())
	if err != nil {
		t.Errorf("missing queryid: unexpected err %v", err)
	}
	if ok {
		t.Error("expected ok=false for a queryid with no samples")
	}
	_, ok2, err := WindowedLatencyMsBetween(ctx, pool, int64(-99887766), time.Now().Add(-time.Hour), time.Now())
	if err != nil || ok2 {
		t.Errorf("between missing: ok=%v err=%v", ok2, err)
	}
}
