package autoexplain

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/planhash"
)

// M0 plan_hash: every plan written to sage.explain_cache carries its
// stable fingerprint; a plan that cannot be fingerprinted is still stored
// with a NULL hash instead of being dropped.

const hashablePlan = `[{"Plan": {"Node Type": "Index Scan",
  "Index Name": "t_pkey", "Relation Name": "t", "Total Cost": 8.3},
  "Execution Time": 0.4}]`

func storedHash(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
) *string {
	t.Helper()
	var h *string
	err := pool.QueryRow(ctx, `SELECT plan_hash FROM sage.explain_cache
		WHERE queryid = $1 ORDER BY id DESC LIMIT 1`, qid).Scan(&h)
	if err != nil {
		t.Fatalf("read plan_hash: %v", err)
	}
	return h
}

func cleanExplainRows(t *testing.T, pool *pgxpool.Pool, qids ...int64) {
	t.Helper()
	for _, q := range qids {
		if _, err := pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", q); err != nil {
			t.Fatalf("clean explain_cache: %v", err)
		}
	}
}

func TestStorePlan_RecordsPlanHash(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()
	const qid = int64(999999911)
	cleanExplainRows(t, pool, qid)
	t.Cleanup(func() { cleanExplainRows(t, pool, qid) })

	c := NewCollector(pool, CollectorConfig{CollectIntervalSeconds: 60},
		&Availability{Method: "unavailable"}, func(string, string, ...any) {})
	if err := c.storePlan(ctx, qid, "SELECT 1", []byte(hashablePlan),
		"auto_explain", 8.3, 0.4); err != nil {
		t.Fatalf("storePlan: %v", err)
	}
	want, err := planhash.Compute([]byte(hashablePlan))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got := storedHash(t, ctx, pool, qid); got == nil || *got != want {
		t.Fatalf("plan_hash = %v, want %s", got, want)
	}
}

func TestStorePlan_UnhashablePlanStoredWithNullHash(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()
	const qid = int64(999999912)
	cleanExplainRows(t, pool, qid)
	t.Cleanup(func() { cleanExplainRows(t, pool, qid) })

	c := NewCollector(pool, CollectorConfig{CollectIntervalSeconds: 60},
		&Availability{Method: "unavailable"}, func(string, string, ...any) {})
	if err := c.storePlan(ctx, qid, "SELECT 1",
		[]byte(`[{"Plan": {"Total Cost": 1.0}}]`), "auto_explain",
		1.0, 0); err != nil {
		t.Fatalf("storePlan must keep plans it cannot fingerprint: %v", err)
	}
	if got := storedHash(t, ctx, pool, qid); got != nil {
		t.Fatalf("plan_hash = %q, want NULL for a plan without Node Type", *got)
	}
}

func TestStoreObservedPlan_RecordsPlanHash(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()
	const qid = int64(999999913)
	cleanExplainRows(t, pool, qid)
	t.Cleanup(func() { cleanExplainRows(t, pool, qid) })

	p := ObservedPlan{QueryID: qid, Query: "SELECT 1",
		CapturedAt: time.Now().UTC().Truncate(time.Microsecond),
		JSON:       []byte(hashablePlan), TotalCost: 8.3, ExecutionMS: 0.4}
	if err := StoreObservedPlan(ctx, pool, p); err != nil {
		t.Fatalf("StoreObservedPlan: %v", err)
	}
	want, _ := planhash.Compute([]byte(hashablePlan))
	if got := storedHash(t, ctx, pool, qid); got == nil || *got != want {
		t.Fatalf("plan_hash = %v, want %s", got, want)
	}
}
