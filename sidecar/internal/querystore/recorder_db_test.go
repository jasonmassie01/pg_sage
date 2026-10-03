package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lo, hi int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_store
		WHERE queryid BETWEEN $1 AND $2`, lo, hi).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func clearQueryIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lo, hi int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_store
		WHERE queryid BETWEEN $1 AND $2`, lo, hi); err != nil {
		t.Fatalf("clear: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.query_store
			WHERE queryid BETWEEN $1 AND $2`, lo, hi)
	})
}

// M11 (dogfood lifeos): 290 queries were written every minute, changed or
// not. 100 idle queries over an hour of one-minute cycles now write one
// row each; one moving query writes every cycle.
func TestRecorder_IdleAndMovingQueriesAgainstPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const lo, hi = int64(7_100_000_000), int64(7_100_000_100)
	clearQueryIDs(t, ctx, pool, lo, hi)
	r := NewRecorder()
	db := &countingExec{inner: pool}
	start := time.Now()
	for minute := 0; minute < 60; minute++ {
		samples := make([]Sample, 0, 101)
		for q := lo; q < lo+100; q++ {
			samples = append(samples, idle(q))
		}
		samples = append(samples, Sample{QueryID: hi, Calls: int64(minute + 1),
			TotalExecMs: float64(minute+1) * 2, MeanExecMs: 2, Rows: 1})
		if _, err := r.Record(ctx, db, samples,
			start.Add(time.Duration(minute)*time.Minute)); err != nil {
			t.Fatalf("minute %d: %v", minute, err)
		}
	}
	if got := countRows(t, ctx, pool, lo, lo+99); got != 100 {
		t.Fatalf("idle queries wrote %d rows over 60 cycles, want 100", got)
	}
	if got := countRows(t, ctx, pool, hi, hi); got != 60 {
		t.Fatalf("moving query wrote %d rows, want 60", got)
	}
	if db.n != 60 {
		t.Fatalf("%d statements for 60 cycles, want one per cycle", db.n)
	}
}

// countingExec counts statements sent to a real pool.
type countingExec struct {
	inner Execer
	n     int
}

func (c *countingExec) Exec(ctx context.Context, sql string, args ...any) (
	pgconn.CommandTag, error) {
	c.n++
	return c.inner.Exec(ctx, sql, args...)
}

// The batched insert stamps every sample with its queryid's latest captured
// plan fingerprint once, and leaves it NULL where none was captured.
func TestRecord_StampsLatestPlanHashPerQuery(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const withPlan, withoutPlan = int64(7_200_000_001), int64(7_200_000_002)
	clearQueryIDs(t, ctx, pool, withPlan, withoutPlan)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.explain_cache WHERE queryid = $1`,
		withPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, captured_at, plan_hash) VALUES
		($1, 'q', '[]', 'test', now() - interval '2 hours', 'old'),
		($1, 'q', '[]', 'test', now() - interval '1 hour', 'new'),
		($1, 'q', '[]', 'test', now(), NULL)`, withPlan); err != nil {
		t.Fatalf("seed plans: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.explain_cache WHERE queryid = $1`, withPlan)
	})
	if err := Record(ctx, pool, []Sample{idle(withPlan), idle(withoutPlan)}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT queryid, plan_hash FROM sage.query_store
		WHERE queryid IN ($1, $2) ORDER BY queryid`, withPlan, withoutPlan)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]*string{}
	for rows.Next() {
		var q int64
		var h *string
		if err := rows.Scan(&q, &h); err != nil {
			t.Fatal(err)
		}
		got[q] = h
	}
	if len(got) != 2 || got[withPlan] == nil || *got[withPlan] != "new" ||
		got[withoutPlan] != nil {
		t.Fatalf("plan hashes = %v, want new and NULL", got)
	}
}

// insertAt writes one sample with an explicit capture time.
func insertAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, calls int64, total float64, epoch *time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
		(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows, stats_epoch)
		VALUES ($1, $2, $3, $4, 0, 0, $5)`, qid, at, calls, total, epoch); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

// Samples are written only when counters move, so the last sample before a
// window is the window's starting point: an idle query has no sample at
// the window start, yet its latency over the window is measurable.
func TestEvidence_AnchorsOnLastSampleBeforeWindow(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(7_300_000_001)
	clearQueryIDs(t, ctx, pool, qid, qid)
	epoch := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	now := time.Now().UTC()
	insertAt(t, ctx, pool, qid, now.Add(-50*time.Minute), 100, 1000, &epoch)
	insertAt(t, ctx, pool, qid, now.Add(-10*time.Minute), 200, 4000, &epoch)
	ev, err := WindowedLatencyEvidence(ctx, pool, qid, now.Add(-30*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != EvidenceMeasured || ev.Samples != 2 || ev.LatencyMs < 29.9 ||
		ev.LatencyMs > 30.1 {
		t.Fatalf("evidence = %+v, want 30 ms measured from the anchor", ev)
	}
	// No movement inside the window: the anchor alone says "no new calls".
	ev, err = WindowedLatencyEvidence(ctx, pool, qid, now.Add(-5*time.Minute), now)
	if err != nil || ev.Status != EvidenceInsufficientSamples {
		t.Fatalf("idle window = %+v, %v; want insufficient (anchor only)", ev, err)
	}
}

// An anchor older than AnchorLookback is not evidence of the window start:
// the query left the sampled set, its counters there are unknown.
func TestEvidence_IgnoresAnchorBeyondLookback(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(7_300_000_002)
	clearQueryIDs(t, ctx, pool, qid, qid)
	now := time.Now().UTC()
	from := now.Add(-30 * time.Minute)
	insertAt(t, ctx, pool, qid, from.Add(-AnchorLookback-time.Minute), 100, 1000, nil)
	insertAt(t, ctx, pool, qid, now.Add(-10*time.Minute), 200, 4000, nil)
	ev, err := WindowedLatencyEvidence(ctx, pool, qid, from, now)
	if err != nil || ev.Status != EvidenceInsufficientSamples || ev.Samples != 1 {
		t.Fatalf("evidence = %+v, %v; want one sample, insufficient", ev, err)
	}
}

// An anchor from another statistics epoch is never differenced (R10).
func TestEvidence_AnchorFromAnotherEpochIsAReset(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	const qid = int64(7_300_000_003)
	clearQueryIDs(t, ctx, pool, qid, qid)
	now := time.Now().UTC()
	old, cur := now.Add(-72*time.Hour).Truncate(time.Second), now.Add(-time.Hour).
		Truncate(time.Second)
	insertAt(t, ctx, pool, qid, now.Add(-50*time.Minute), 100, 1000, &old)
	insertAt(t, ctx, pool, qid, now.Add(-10*time.Minute), 500, 9000, &cur)
	ev, err := WindowedLatencyEvidence(ctx, pool, qid, now.Add(-30*time.Minute), now)
	if err != nil || ev.Status != EvidenceCountersReset {
		t.Fatalf("evidence = %+v, %v; want counters_reset", ev, err)
	}
}
