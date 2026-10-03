package api

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offender on sage.action_log (perf-selfexcl): the actions
// list's capped total, `SELECT 1 FROM sage.action_log WHERE true LIMIT
// 1001`, was a sequential scan of the ledger (1,001 rows of it). It now
// walks idx_action_log_time, with or without a time window, in the
// generic plan the prepared statement falls back to.
func TestActionLogCappedCountWalksTheTimeIndex(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	phase2CleanTables(t, pool, ctx)
	t.Cleanup(func() {
		// Leave no 20,000-row ledger or its statistics to later plan tests.
		phase2CleanTables(t, pool, ctx)
		_, _ = pool.Exec(ctx, "ANALYZE sage.action_log")
	})
	seedActionLog(t, pool, "SELECT 1", 20000, keysetBase, time.Second)
	if _, err := pool.Exec(ctx, "VACUUM (ANALYZE) sage.action_log"); err != nil {
		t.Fatal(err)
	}
	from, to := keysetBase.Add(time.Hour), keysetBase.Add(2*time.Hour)
	var stmts []perfgate.Statement
	for _, w := range [][2]time.Time{{}, {from, {}}, {{}, to}, {from, to}} {
		window, _ := timeWindowSQL("executed_at", w[0], w[1], nil)
		stmts = append(stmts, perfgate.Statement{Query: cappedCountSQL(actionLogCountFrom(window))})
	}
	plans, err := perfgate.ExplainStatements(ctx, pool, stmts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Err != "" || len(p.SeqScans) != 0 {
			t.Errorf("capped count %q: seq scans %v, err %q", p.Statement.Query, p.SeqScans, p.Err)
		}
	}
	n, err := cappedCount(ctx, pool, actionLogCountFrom(""), nil)
	if err != nil || n != maxListTotal+1 {
		t.Fatalf("capped count = %d (%v), want %d", n, err, maxListTotal+1)
	}
	window, args := timeWindowSQL("executed_at", from, to, nil)
	n, err = cappedCount(ctx, pool, actionLogCountFrom(window), args)
	if err != nil || n != maxListTotal+1 {
		t.Fatalf("windowed capped count = %d (%v), want %d (3,601 rows in the hour)", n,
			err, maxListTotal+1)
	}
}
