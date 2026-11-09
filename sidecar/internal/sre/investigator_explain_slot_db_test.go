package sre

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-07: the investigator's EXPLAIN holds the database's probe slot, has
// its own pool-acquire budget, takes the statement text from the
// collector's in-memory snapshot (never the text-loading showtext read),
// refuses a data-modifying CTE before PREPARE takes its locks, and reports
// a missing SELECT grant as no_privilege.

func TestStatementExplainer_WaitsForTheDatabaseSlot(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 3)
	runner := probes.NewRunner(pool, probes.Catalog(), nil)
	ex := NewStatementExplainer(runner, cachedTexts(t, ctx, pool, id))
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runner.WithSlot(ctx, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	start := time.Now()
	res := ex.ExplainStatement(ctx, id)
	waited := time.Since(start)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder: %v", err)
	}
	if res.Status != probes.StatusError || res.Reason != "concurrency_limit" ||
		res.Phase != probes.PhaseQueue {
		t.Fatalf("explain behind a held slot = %+v, want error concurrency_limit "+
			"in %s", res, probes.PhaseQueue)
	}
	if waited < 4*time.Second {
		t.Fatalf("gave up after %s, want it to wait the queue budget", waited)
	}
	if res := ex.ExplainStatement(ctx, id); res.Status != probes.StatusOK {
		t.Fatalf("explain after the slot is free = %+v, want ok", res)
	}
}

func TestStatementExplainer_PoolWaitHasItsOwnBudget(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 4)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer one.Close()
	busy, err := one.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	start := time.Now()
	res := newExplainer(one, cachedTexts(t, ctx, pool, id)).ExplainStatement(ctx, id)
	waited := time.Since(start)
	busy.Release()
	if res.Status != probes.StatusError || res.Phase != probes.PhasePoolAcquire {
		t.Fatalf("explain on a full pool = %+v, want an error in %s", res,
			probes.PhasePoolAcquire)
	}
	// The pool wait is the runner's acquire budget, not the statement's
	// 1.5 s budget.
	if limit := probes.MaxStatementTimeout + time.Second; waited <= limit {
		t.Fatalf("pool wait ended after %s, want the acquire budget (over %s)",
			waited, limit)
	}
	budget := probes.NewRunner(one, probes.Catalog(), nil).AcquireWait()
	if waited < budget-250*time.Millisecond || waited > budget+2*time.Second {
		t.Fatalf("pool wait ended after %s, want about the acquire budget %s",
			waited, budget)
	}
}

func TestStatementExplainer_NoCachedTextIsUnsupported(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 5)
	sources := map[string]StatementTexts{
		"no text source": nil,
		"not cached":     func(int64) (string, bool) { return "", false },
		"empty text":     func(int64) (string, bool) { return "", true },
	}
	for name, texts := range sources {
		res := newExplainer(pool, texts).ExplainStatement(ctx, id)
		if res.Status != probes.StatusUnsupported || res.Reason != ReasonTextNotCached ||
			len(res.Rows) != 0 {
			t.Errorf("%s: explain = %+v, want unsupported %s", name, res,
				ReasonTextNotCached)
		}
	}
}

// targetLocks counts the locks on tbl held or awaited by any session but
// skip's.
func targetLocks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tbl string,
	skip uint32) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE relation = $1::regclass AND pid <> $2`, tbl, int64(skip)).Scan(&n); err != nil {
		t.Fatalf("pg_locks: %v", err)
	}
	return n
}

func TestStatementExplainer_RefusesADataModifyingCTEBeforeItLocks(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "WITH d AS (DELETE FROM "+tbl+
		" WHERE id = 999999 RETURNING id) SELECT id FROM d", "DELETE FROM "+tbl+" WHERE")
	// A SHARE lock conflicts with the RowExclusiveLock a PREPARE of the
	// DELETE would take: reaching PREPARE would end in lock_timeout.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx, "LOCK TABLE "+tbl+" IN SHARE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	blockerPID := blocker.Conn().PgConn().PID()
	res := newExplainer(pool, cachedTexts(t, ctx, pool, id)).ExplainStatement(ctx, id)
	if res.Status != probes.StatusUnsupported || res.Reason != ReasonNotReadStatement ||
		len(res.Rows) != 0 {
		t.Fatalf("explain of a DELETE CTE = %+v, want unsupported %s", res,
			ReasonNotReadStatement)
	}
	if n := targetLocks(t, ctx, pool, tbl, blockerPID); n != 0 {
		t.Fatalf("%d locks on %s besides the blocker's, want 0", n, tbl)
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&rows); err != nil ||
		rows != 50 {
		t.Fatalf("rows after = %d (%v), want 50", rows, err)
	}
}

func TestStatementExplainer_MonitorOnlyRoleIsMissingSelect(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 6)
	role, password := tbl+"_monitor", fmt.Sprintf("pw-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+
		"' IN ROLE pg_monitor"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, password
	monitor, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(func() {
		monitor.Close()
		_, _ = pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	res := newExplainer(monitor, cachedTexts(t, ctx, pool, id)).ExplainStatement(ctx, id)
	if res.Status != probes.StatusNoPrivilege || res.Reason != ReasonMissingSelect ||
		len(res.Rows) != 0 {
		t.Fatalf("explain as a pg_monitor-only role = %+v, want no_privilege %s", res,
			ReasonMissingSelect)
	}
	if !strings.Contains(res.Error, "permission denied") {
		t.Fatalf("error = %q, want the server's permission error", res.Error)
	}
}

// fillStatements records n distinct statements with long texts in
// pg_stat_statements (track = all, so the DO block's statements count).
func fillStatements(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET pg_stat_statements.track = 'all'"); err != nil {
		t.Fatalf("track all statements (the test needs a superuser): %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET pg_stat_statements.track") }()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DO $$
DECLARE k int; m int; q text; i int; done int := 0;
BEGIN
  FOR k IN 1..100 LOOP
    FOR m IN 1..100 LOOP
      EXIT WHEN done >= %d;
      q := 'SELECT v.id';
      FOR i IN 1..m LOOP q := q || ', v.id * ' || i || ' + ' || i || ' AS sre_x_' || i; END LOOP;
      q := q || ' FROM (VALUES (1)) v(id) WHERE v.id = 0';
      FOR i IN 1..k LOOP q := q || ' OR v.id = ' || i; END LOOP;
      EXECUTE q;
      done := done + 1;
    END LOOP;
  END LOOP;
END $$`, n)); err != nil {
		t.Fatalf("fill pg_stat_statements: %v", err)
	}
}

// explainerReads lists the explainer's own pg_stat_statements reads as
// pg_stat_statements tracked them.
func explainerReads(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT query FROM pg_stat_statements
		WHERE query LIKE $1 AND query LIKE $2`, "%pg_stat_statements(%", "%s.queryid = $1%")
	if err != nil {
		t.Fatalf("tracked reads: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, q)
	}
	return out
}

func TestStatementExplainer_StaysFastWithManyLongStatements(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	tbl := explainTable(t, ctx, pool)
	fillStatements(t, ctx, pool, 3000)
	id := statementID(t, ctx, pool, "SELECT v FROM "+tbl+" WHERE id = $1",
		"FROM "+tbl+" WHERE id", 8)
	ex := newExplainer(pool, cachedTexts(t, ctx, pool, id))
	var best probes.Result
	for i := 0; i < 3; i++ {
		res := ex.ExplainStatement(ctx, id)
		if res.Status != probes.StatusOK {
			t.Fatalf("explain run %d = %+v", i+1, res)
		}
		if i == 0 || res.ElapsedMS < best.ElapsedMS {
			best = res
		}
	}
	if limit := probes.MaxStatementTimeout / 2; time.Duration(best.ElapsedMS)*
		time.Millisecond >= limit {
		t.Fatalf("explain took %d ms with thousands of long statements, want under %s",
			best.ElapsedMS, limit)
	}
	reads := explainerReads(t, ctx, pool)
	if len(reads) == 0 {
		t.Fatal("the explainer's queryid check is not in pg_stat_statements")
	}
	// pg_stat_statements normalizes the boolean to $n, so the tracked text
	// shows the call's form: the named showtext argument, never the
	// positional (showtext = true) call the text-loading read used.
	for _, q := range reads {
		if strings.Contains(q, "pg_stat_statements($") ||
			!strings.Contains(q, "pg_stat_statements(showtext => $") {
			t.Fatalf("the explainer read %q, want the showtext => false call only", q)
		}
	}
}
