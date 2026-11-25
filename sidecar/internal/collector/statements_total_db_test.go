package collector

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// evid-01a on a real server: StatementsTotalExecMs, StatementsCalls and
// StatementsEntries equal the sums over this database's pg_stat_statements
// entries less pg_sage's own, read directly, and the 500 cap does not
// touch them (600 statements). The collector's pool is tagged as in
// production: on PostgreSQL 18 an untagged pool's statements lose their
// leading tag, none is known as pg_sage's own, and the totals read counts
// in the view one call more than it saw of itself. The database is on the
// exclusive server (ci-23): on the shared one other packages' statements
// fill the cluster-wide hash and evict some of the 600, once-called rows.

func TestStatementTotals_EqualTheViewUnaffectedByTheCap(t *testing.T) {
	ctx := context.Background()
	pool := exclusiveTextTestDB(t, "pg_stat_statements entries of one database")
	// The tagged pool's pinned search_path does not resolve the view bare.
	requireStatStatements(t, ctx, New(pool, testConfig(), 0, noopLog))
	c := New(taggedPool(t, ctx, pool), testConfig(), 0, noopLog)
	c.cfg.Collector.MaxQueries = 500
	// The collector's reads run in transactions whose COMMIT lands after
	// the read and is no pg_sage statement anyone can tell apart from an
	// application's: count statements only, so the comparison is exact.
	mustExec(t, pool, "DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET "+
		"pg_stat_statements.track_utility = off', current_database()); END $$")
	pool.Reset()
	for i := 1; i <= 600; i++ {
		mustExec(t, pool, "SELECT 1 AS w"+strings.Repeat(", 1", i))
	}
	var totals statementTotals
	cycle := func() []QueryStats {
		c.stmtTexts.observeEpoch(c.collectStatementsEpoch(ctx))
		qs, err := c.queriesVia(ctx, timedCatalogQuerier{collector: c}, &totals)
		if err != nil {
			t.Fatalf("collect queries: %v", err)
		}
		return qs
	}
	for range 3 { // learn pg_sage's own statements
		cycle()
	}
	qs := cycle()
	if len(qs) != 500 {
		t.Fatalf("admitted %d queries, want the 500 cap", len(qs))
	}
	exec, calls, entries := viewTotals(t, ctx, c, pool)
	if !totals.known || totals.entries != entries || totals.calls != calls {
		t.Fatalf("totals %+v, view: entries %d calls %d", totals, entries, calls)
	}
	if entries < 600 {
		t.Fatalf("view holds %d entries, want the 600 statements and more", entries)
	}
	if exec <= 0 || math.Abs(totals.execMs-exec)/exec > 1e-9 {
		t.Fatalf("total exec %v ms, view %v ms", totals.execMs, exec)
	}
}

// viewTotals reads the same totals directly from pg_stat_statements,
// leaving out the queryids the collector knows as its own.
func viewTotals(t *testing.T, ctx context.Context, c *Collector,
	pool *pgxpool.Pool) (exec float64, calls int64, entries int) {
	t.Helper()
	names, err := c.pgss()
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `SELECT COALESCE(sum(total_exec_time), 0)::float8,
		COALESCE(sum(calls), 0)::int8, count(*)::int FROM (
		SELECT queryid, sum(total_exec_time) AS total_exec_time, sum(calls) AS calls
		  FROM `+names.fn+`(false)
		 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		   AND queryid IS NOT NULL AND queryid <> ALL($1::int8[])
		 GROUP BY queryid) e`, nonNil(c.stmtTexts.ownIDs())).Scan(&exec, &calls, &entries)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	return exec, calls, entries
}
