package analyzer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender on sage.action_log (perf-selfexcl): the
// app-managed-index check read pg_sage's index drops of 180 days with no
// index on action_type, a sequential scan of the whole ledger every
// analyzer cycle (7 of 20,000 rows in the small gate). pg_sage's drops
// are rare; reading them is an index range of the drops only.
func TestAppManagedIndexesReadDropsByIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "app_managed_scan"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:app_managed_scan")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		rollback_sql, outcome) VALUES ('drop_index', 'DROP INDEX CONCURRENTLY public.ix_gone',
		'CREATE INDEX ix_gone ON public.t (a)', 'success')`); err != nil {
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
	before, _ := testdb.XactScansOf(ctx, tx, "sage.action_log")
	for range 7 { // the sixth run of a cached statement uses its generic plan
		if _, err := loadAppManagedIndexes(ctx, tx); err != nil {
			t.Fatalf("loadAppManagedIndexes: %v", err)
		}
	}
	after, _ := testdb.XactScansOf(ctx, tx, "sage.action_log")
	// At most two fetches per read: the one drop, plus a probe the planner
	// may make for the actual end of the executed_at range while estimating
	// it (PG14 counted 8 for 7 reads). A full ledger read is 20,000.
	if d := after.Minus(before); d.Seq != 0 || d.IndexFetch > 14 {
		t.Fatalf("7 reads of the drop history: %+v on sage.action_log, want no seq scan "+
			"and only the one drop fetched per read (plus planner probes)", d)
	}
}
