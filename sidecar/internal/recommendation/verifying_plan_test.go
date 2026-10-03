package recommendation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender (perf-selfexcl, after the first fixes): the
// reconcile's read of verifying recommendations scanned sage.action_log
// once per cycle (7 seq scans of 20,000 rows in the small gate). Its
// generic plan must reach each recommendation's action and verification
// by index, and running it as a prepared statement (pgx caches it, so the
// sixth run uses the generic plan) never scans the ledger.
func TestVerifyingEffectsReachActionsByIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "verifying_plan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:verifying_plan")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	plans, err := perfgate.ExplainStatements(ctx, pool,
		[]perfgate.Statement{{Query: verifyingEffectsSQL}})
	if err != nil || len(plans) != 1 {
		t.Fatalf("explain: %v (%d plans)", err, len(plans))
	}
	if plans[0].Err != "" || len(plans[0].SeqScans) != 0 {
		t.Fatalf("verifying effects generic plan: seq scans %v, err %q",
			plans[0].SeqScans, plans[0].Err)
	}
	s := NewStore(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, _ := testdb.XactScansOf(ctx, tx, "sage.action_log")
	for range 7 {
		if _, err := s.verifyingEffectsOn(ctx, tx, "verifying_plan"); err != nil {
			t.Fatalf("verifying effects: %v", err)
		}
	}
	after, _ := testdb.XactScansOf(ctx, tx, "sage.action_log")
	if d := after.Minus(before); d.Seq != 0 {
		t.Fatalf("7 reconcile reads scanned sage.action_log %d times (%d rows)", d.Seq,
			d.SeqRead)
	}
}
