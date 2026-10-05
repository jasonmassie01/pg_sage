package earned

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf regression (2026-10-04): the Trust view's family safety read
// fetched every outcome of the window (20,000 rows in the small perf
// gate, two reads per view) to count the harmful ones. On a seeded
// history it touches only what it returns; the reconciler's read of
// decided verdicts stays on its decided_at index (trust ledger migration)
// now that the gate seeds sage.action_outcome.

func seededLedgerPool(t *testing.T, name string) (*pgxpool.Pool, perfgate.Binding) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	own := perfgate.NewBinding("startup:" + name)
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(), own); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, own
}

// scansIn runs fn in a transaction and returns table's scan counters.
func scansIn(t *testing.T, pool *pgxpool.Pool, table string,
	fn func(ctx context.Context, tx pgx.Tx)) testdb.XactScans {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := testdb.XactScansOf(ctx, tx, table)
	if err != nil {
		t.Fatal(err)
	}
	fn(ctx, tx)
	after, err := testdb.XactScansOf(ctx, tx, table)
	if err != nil {
		t.Fatal(err)
	}
	return after.Minus(before)
}

func TestSafetyReadFetchesOnlyHarmfulOutcomes(t *testing.T) {
	pool, own := seededLedgerPool(t, "trust_safety_plan")
	var db string
	if err := pool.QueryRow(context.Background(),
		"SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	families := make([]string, 0, len(AllFamilies()))
	for _, f := range AllFamilies() {
		families = append(families, string(f))
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	d := scansIn(t, pool, "sage.sre_autonomy_outcomes", func(ctx context.Context, tx pgx.Tx) {
		for range 7 { // the sixth run uses the generic plan
			rows, err := tx.Query(ctx, safetySetSQL, own.DeploymentID, db, since, families)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
		}
	})
	if d.Seq != 0 || d.IndexFetch > 100 {
		t.Fatalf("7 safety reads: %d seq scans, %d index fetches; the seeded window "+
			"holds no harmful outcome, so none should be fetched", d.Seq, d.IndexFetch)
	}
}

func TestDecidedVerdictsReadUsesAnIndex(t *testing.T) {
	pool, _ := seededLedgerPool(t, "verdict_facts_plan")
	var rowsInTable int64
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.action_outcome").Scan(&rowsInTable); err != nil {
		t.Fatal(err)
	}
	if rowsInTable < 5000 {
		t.Fatalf("seeded sage.action_outcome has %d rows; the test needs history", rowsInTable)
	}
	d := scansIn(t, pool, "sage.action_outcome", func(ctx context.Context, tx pgx.Tx) {
		for range 7 {
			rows, err := tx.Query(ctx, verdictFactsSQL, nil,
				(30 * 24 * time.Hour).Seconds(), selfBatch)
			if err != nil {
				t.Fatal(err)
			}
			rows.Close()
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
		}
	})
	if d.Seq != 0 {
		t.Fatalf("7 verdict reads scanned sage.action_outcome %d times (%d rows)", d.Seq,
			d.SeqRead)
	}
}

// Perf gate offender (2026-10-04, once sage.action_outcome was seeded): the
// reconciler's rolled-back read joined sage.action_outcome; its generic
// plan (pgx prepares it, so PostgreSQL switches to the generic plan after
// five runs) cannot estimate measured_at >= COALESCE($1, ...) and hashed
// the whole table, 2 seq scans of 20,000 rows per steady phase. No fact
// read of the trust reconciler may plan a scan of the outcome table.
func TestReconcilerFactReadsNeverScanOutcomes(t *testing.T) {
	pool, _ := seededLedgerPool(t, "reconcile_facts_plan")
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	stmts := []perfgate.Statement{{Query: verdictFactsSQL}, {Query: rollbackFactsSQL},
		{Query: handoffSQL}, {Query: autoExecutionSQL}}
	plans, err := perfgate.ExplainStatements(context.Background(), pool, stmts)
	if err != nil || len(plans) != len(stmts) {
		t.Fatalf("explain: %v (%d plans)", err, len(plans))
	}
	for _, p := range plans {
		if p.Err != "" {
			t.Errorf("%.60s: %s", p.Statement.Query, p.Err)
		}
		for _, s := range p.SeqScans {
			if s.Relation == "action_outcome" || s.Relation == "action_log" {
				t.Errorf("generic plan of %.80s scans sage.%s", p.Statement.Query, s.Relation)
			}
		}
	}
}
