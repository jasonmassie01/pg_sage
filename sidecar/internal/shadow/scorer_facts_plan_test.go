package shadow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender (2026-10-04, once sage.action_outcome was seeded):
// the scorer's read of recent actions LEFT JOINed the outcome table and,
// for a window of a few days (1,500 actions), hashed all 20,000 outcomes:
// a seq scan per scorer pass that grows with every verified action. Each
// action's verdict is one primary-key read, whatever the window.
func TestScorerFactReadsNeverScanTheOutcomeTable(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "scorer_facts_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:scorer_facts_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, _ := testdb.XactScansOf(ctx, tx, "sage.action_outcome")
	for _, days := range []int{1, 6, 30} {
		since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		for _, q := range []string{actionFactsSQL, queueFactsSQL} {
			rows, err := tx.Query(ctx, q, since, factBatch)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
		}
	}
	after, _ := testdb.XactScansOf(ctx, tx, "sage.action_outcome")
	if d := after.Minus(before); d.Seq != 0 {
		t.Fatalf("scorer fact reads scanned sage.action_outcome %d times (%d rows)", d.Seq,
			d.SeqRead)
	}
}
