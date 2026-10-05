package shadow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf regression (2026-10-04): the Trust page's shadow summary read
// sage.shadow_decision whole (its sql, prediction and reason columns
// included) on every view. It now reads a narrow covering index, in the
// order it groups by, and never the table itself.
func TestSummaryNeverScansTheDecisionTable(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "shadow_summary_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:shadow_summary_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)
	want, err := s.Summary(ctx)
	if err != nil || len(want) == 0 {
		t.Fatalf("summary = %+v, %v; want the seeded classes", want, err)
	}
	total := 0
	for _, c := range want {
		total += c.Total
	}
	if total < 2000 {
		t.Fatalf("summary counts %d decisions, want the 2000 seeded", total)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, _ := testdb.XactScansOf(ctx, tx, "sage.shadow_decision")
	for range 7 { // the sixth run uses the generic plan
		rows, err := tx.Query(ctx, summarySQL, "")
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	after, _ := testdb.XactScansOf(ctx, tx, "sage.shadow_decision")
	if d := after.Minus(before); d.Seq != 0 {
		t.Fatalf("7 summaries scanned sage.shadow_decision %d times (%d rows)", d.Seq,
			d.SeqRead)
	}
}
