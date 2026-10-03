package slo

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertSample writes one query's sample of a capture.
func insertSample(t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time,
	qid, calls int64, total float64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		    total_exec_time, mean_exec_time, stats_epoch)
		VALUES ($1, $2, $3, $4, 0, $5::timestamptz)`, at, qid, calls, total, epoch); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

// query_store keeps a sample only when a query's counters moved (perf M11).
// A query idle in the previous capture still has an interval: from its own
// last sample. Here q2 last moved three minutes ago and q3 not at all.
func TestLatencyProxy_QueriesIdleInThePreviousCaptureStillCount(t *testing.T) {
	pool, ctx := livePool(t)
	clearCaptures(t, ctx, pool)
	now := time.Now()
	a, b, c := now.Add(-180*time.Second), now.Add(-120*time.Second), now.Add(-60*time.Second)
	insertSample(t, ctx, pool, a, 9001, 1000, 10000)
	insertSample(t, ctx, pool, a, 9002, 50, 1000)
	insertSample(t, ctx, pool, a, 9003, 7, 70)
	insertSample(t, ctx, pool, b, 9001, 1100, 11000)
	insertSample(t, ctx, pool, c, 9001, 1200, 12000) // 100 calls at 10 ms
	insertSample(t, ctx, pool, c, 9002, 60, 3000)    // 10 calls at 200 ms since a
	s := NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, now,
		fakeHistory{median: 10, n: 100})
	if s.Value == nil || *s.Value != 200 || s.Eligible != 1 || s.Bad != 1 {
		t.Fatalf("slice = %+v value=%v, want 200 ms (q2 measured from its own last sample)",
			s, s.Value)
	}
}

// A query seen for the first time in the latest capture has no interval.
func TestLatencyProxy_FirstSampleOfAQueryHasNoInterval(t *testing.T) {
	pool, ctx := livePool(t)
	clearCaptures(t, ctx, pool)
	now := time.Now()
	insertSample(t, ctx, pool, now.Add(-120*time.Second), 9001, 1000, 10000)
	insertSample(t, ctx, pool, now.Add(-60*time.Second), 9001, 1100, 11000)
	insertSample(t, ctx, pool, now.Add(-60*time.Second), 9002, 500, 500000) // new
	s := NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, now,
		fakeHistory{median: 10, n: 100})
	if s.Value == nil || *s.Value != 10 || s.Bad != 0 {
		t.Fatalf("slice = %+v value=%v, want 10 ms from q1 only", s, s.Value)
	}
}
