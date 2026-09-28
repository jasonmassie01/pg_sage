package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Plan fingerprints (M0 plan_hash): every query_store sample records the
// fingerprint of the latest plan captured for its queryid, so a plan flip
// shows up in the per-queryid time series.

func seedPlanHash(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	qid int64, hash string, at time.Time,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, captured_at, plan_hash)
		VALUES ($1, 'select 1', '{"Plan": {"Node Type": "Result"}}'::jsonb,
		        'test', $2, NULLIF($3, ''))`, qid, at, hash)
	if err != nil {
		t.Fatalf("seed explain_cache: %v", err)
	}
}

func latestSampleHash(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
) *string {
	t.Helper()
	var h *string
	err := pool.QueryRow(ctx, `SELECT plan_hash FROM sage.query_store
		WHERE queryid = $1 ORDER BY id DESC LIMIT 1`, qid).Scan(&h)
	if err != nil {
		t.Fatalf("read query_store sample: %v", err)
	}
	return h
}

func cleanPlanHashRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, qids ...int64,
) {
	t.Helper()
	for _, q := range qids {
		for _, table := range []string{"query_store", "explain_cache"} {
			if _, err := pool.Exec(ctx, "DELETE FROM sage."+table+
				" WHERE queryid = $1", q); err != nil {
				t.Fatalf("clean %s: %v", table, err)
			}
		}
	}
}

func TestRecord_StampsLatestCapturedPlanHash(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(pool.Close)
	const qid, other = int64(515151515151), int64(515151515152)
	cleanPlanHashRows(t, ctx, pool, qid, other)
	t.Cleanup(func() { cleanPlanHashRows(t, ctx, pool, qid, other) })

	now := time.Now()
	seedPlanHash(t, ctx, pool, qid, "v1:old", now.Add(-2*time.Hour))
	seedPlanHash(t, ctx, pool, qid, "v1:new", now.Add(-time.Minute))
	// A newer row without a fingerprint (legacy capture) is ignored.
	seedPlanHash(t, ctx, pool, qid, "", now.Add(-time.Second))
	seedPlanHash(t, ctx, pool, other, "v1:other", now)

	if err := Record(ctx, pool, []Sample{
		{QueryID: qid, Calls: 10, TotalExecMs: 50, MeanExecMs: 5},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := latestSampleHash(t, ctx, pool, qid)
	if got == nil || *got != "v1:new" {
		t.Fatalf("plan_hash = %v, want v1:new (latest non-null capture "+
			"for this queryid only)", deref(got))
	}
}

// A plan flip between two samples is visible as two different hashes in
// consecutive rows, which is what plan-regression detection reads.
func TestRecord_PlanFlipVisibleAcrossSamples(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(pool.Close)
	const qid = int64(515151515153)
	cleanPlanHashRows(t, ctx, pool, qid)
	t.Cleanup(func() { cleanPlanHashRows(t, ctx, pool, qid) })

	seedPlanHash(t, ctx, pool, qid, "v1:before", time.Now().Add(-time.Hour))
	if err := Record(ctx, pool, []Sample{{QueryID: qid, Calls: 1}}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	seedPlanHash(t, ctx, pool, qid, "v1:after", time.Now())
	if err := Record(ctx, pool, []Sample{{QueryID: qid, Calls: 2}}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT plan_hash FROM sage.query_store
		WHERE queryid = $1 ORDER BY id`, qid)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h *string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		hashes = append(hashes, deref(h))
	}
	if len(hashes) != 2 || hashes[0] != "v1:before" || hashes[1] != "v1:after" {
		t.Fatalf("hash series = %v, want [v1:before v1:after]", hashes)
	}
}

func TestRecord_NoCapturedPlanLeavesHashNull(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(pool.Close)
	const qid = int64(515151515154)
	cleanPlanHashRows(t, ctx, pool, qid)
	t.Cleanup(func() { cleanPlanHashRows(t, ctx, pool, qid) })

	if err := Record(ctx, pool, []Sample{{QueryID: qid, Calls: 3}}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if got := latestSampleHash(t, ctx, pool, qid); got != nil {
		t.Fatalf("plan_hash = %q, want NULL when no plan was captured", *got)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
