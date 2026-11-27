package optimizer_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// indexes-05 happy path: removing the optimizer's plan capture leaves the
// auto_explain write path into sage.explain_cache untouched. (External
// package: autoexplain imports the analyzer, which imports optimizer.)
func TestAutoExplainStillWritesExplainCache(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const qid = int64(88552201)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
	})
	plan := autoexplain.ObservedPlan{QueryID: qid, Query: "SELECT 1", TotalCost: 3,
		CapturedAt: time.Now().Add(-time.Minute),
		JSON:       []byte(`[{"Plan":{"Node Type":"Result","Total Cost":3}}]`)}
	if err := autoexplain.StoreObservedPlan(context.Background(), pool, plan); err != nil {
		t.Fatalf("store observed plan: %v", err)
	}
	var source string
	var cost float64
	if err := pool.QueryRow(context.Background(), `SELECT source, total_cost
		FROM sage.explain_cache WHERE queryid = $1`, qid).Scan(&source, &cost); err != nil {
		t.Fatalf("read explain_cache: %v", err)
	}
	if source != "auto_explain_log" || cost != 3 {
		t.Fatalf("explain_cache row = (%s, %v), want (auto_explain_log, 3)", source, cost)
	}
}
