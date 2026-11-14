package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The operator CREATE INDEX CONCURRENTLY path runs in a database of its own
// per test (executor-11). CREATE INDEX CONCURRENTLY waits for every older
// snapshot in its database, and the package database is shared with every
// other executor test and with their emergency-stop toggling, so a leaked
// session or a stop left on by an earlier test must not decide the result.
// No boundary tests: this path has no thresholds of its own.

// concDB is a freshly bootstrapped database owned by one test.
func concDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "manual_conc")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse manual_conc dsn: %v", err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect manual_conc: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap manual_conc: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config migration manual_conc: %v", err)
	}
	releasePoolAdvisoryLocks(ctx, pool)
	return pool
}

// concTable creates a populated table with a name unique to the test.
func concTable(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	table := fmt.Sprintf("conc_%d", time.Now().UnixNano())
	_, err := pool.Exec(context.Background(), "CREATE TABLE public."+table+
		" AS SELECT g AS id, md5(g::text) AS val FROM generate_series(1, 500) g")
	if err != nil {
		t.Fatalf("create table %s: %v", table, err)
	}
	return table
}

// concFinding inserts an open finding whose recommendation is sql; the
// category carries the SQL's hash so two findings on one table stay
// distinct under the findings dedup key.
func concFinding(t *testing.T, pool *pgxpool.Pool, table, sql string) int {
	t.Helper()
	var id int
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql)
		VALUES ('manual_conc_' || md5($2), 'warning', 'index', $1, 'manual conc', '{}',
		        'rec', $2)
		RETURNING id`, "public."+table, sql).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding for %s: %v", table, err)
	}
	return id
}

// concExecutor is a production executor over pool under a standing gate.
// Its emergency-stop check reads pool's own (fresh, unset) flag.
func concExecutor(t *testing.T, pool *pgxpool.Pool, lockMs int) *Executor {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.DDLTimeoutSeconds = 30
	cfg.Safety.LockTimeoutMs = lockMs
	e := New(pool, cfg, time.Time{}, nopLog)
	withTestStandingGate(e)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return e
}

// concAction is the one action_log row of a finding.
type concAction struct {
	id          int64
	sql         string
	outcome     string
	rollbackSQL string
	remnant     bool
}

// oneAction reads the finding's action rows and fails unless exactly one.
func oneAction(t *testing.T, pool *pgxpool.Pool, findingID int) concAction {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, sql_executed, outcome,
		COALESCE(rollback_sql, ''), COALESCE(before_state ? 'remnant_cleanup', false)
		FROM sage.action_log WHERE finding_id = $1 ORDER BY id`, findingID)
	if err != nil {
		t.Fatalf("read actions of finding %d: %v", findingID, err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (concAction, error) {
		var a concAction
		return a, r.Scan(&a.id, &a.sql, &a.outcome, &a.rollbackSQL, &a.remnant)
	})
	if err != nil {
		t.Fatalf("scan actions of finding %d: %v", findingID, err)
	}
	if len(got) != 1 {
		t.Fatalf("finding %d has %d action rows (%+v), want exactly 1",
			findingID, len(got), got)
	}
	return got[0]
}

// concIndex reports whether public.name exists and is valid.
func concIndex(t *testing.T, pool *pgxpool.Pool, name string) (exists, valid bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT
		EXISTS (SELECT 1 FROM pg_index WHERE indexrelid = to_regclass($1)),
		COALESCE((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)), false)`,
		"public."+name).Scan(&exists, &valid)
	if err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return exists, valid
}

// findingActed reports whether the finding was marked acted on.
func findingActed(t *testing.T, pool *pgxpool.Pool, findingID int) bool {
	t.Helper()
	var acted bool
	if err := pool.QueryRow(context.Background(),
		`SELECT acted_on_at IS NOT NULL FROM sage.findings WHERE id = $1`,
		findingID).Scan(&acted); err != nil {
		t.Fatalf("read finding %d: %v", findingID, err)
	}
	return acted
}

// TestCoverage_ExecuteManual_ConcurrentlyPath verifies that an operator
// CREATE INDEX CONCURRENTLY runs outside a transaction block (inside one
// PostgreSQL refuses it), builds a valid index and records exactly one
// action. The row is inserted as monitoring and settled by the immediate
// verifier: a build has no performance measurement, so it ends
// unverifiable (never success by default) and the finding is acted on.
func TestCoverage_ExecuteManual_ConcurrentlyPath(t *testing.T) {
	pool := concDB(t)
	table := concTable(t, pool)
	index := table + "_id"
	sql := "CREATE INDEX CONCURRENTLY " + index + " ON public." + table + " (id)"
	findingID := concFinding(t, pool, table, sql)
	e := concExecutor(t, pool, 5000)

	actionID, err := e.ExecuteManual(context.Background(), findingID, sql, "", nil)
	if err != nil {
		t.Fatalf("ExecuteManual(CONCURRENTLY): %v", err)
	}
	if actionID <= 0 {
		t.Fatalf("actionID = %d, want the action_log id (> 0)", actionID)
	}
	got := oneAction(t, pool, findingID)
	if got.id != actionID || got.sql != sql || got.outcome != "unverifiable" {
		t.Fatalf("action = %+v, want id %d, sql %q, outcome unverifiable",
			got, actionID, sql)
	}
	if exists, valid := concIndex(t, pool, index); !exists || !valid {
		t.Fatalf("index %s exists=%v valid=%v, want a valid index", index, exists, valid)
	}
	if !findingActed(t, pool, findingID) {
		t.Fatalf("finding %d not marked acted on after a successful build", findingID)
	}
}

// TestCoverage_ExecuteManual_ConcurrentlyRollbackMonitored verifies the
// first state of an action with a rollback: recorded as monitoring with its
// rollback, under the verification window.
func TestCoverage_ExecuteManual_ConcurrentlyRollbackMonitored(t *testing.T) {
	pool := concDB(t)
	table := concTable(t, pool)
	index := table + "_id"
	sql := "CREATE INDEX CONCURRENTLY " + index + " ON public." + table + " (id)"
	rollback := "DROP INDEX CONCURRENTLY IF EXISTS public." + index
	findingID := concFinding(t, pool, table, sql)
	e := concExecutor(t, pool, 5000) // default rollback window: minutes

	actionID, err := e.ExecuteManual(context.Background(), findingID, sql, rollback, nil)
	if err != nil || actionID <= 0 {
		t.Fatalf("ExecuteManual with rollback = %d, %v; want an action id", actionID, err)
	}
	got := oneAction(t, pool, findingID)
	if got.id != actionID || got.outcome != "monitoring" ||
		!strings.Contains(got.rollbackSQL, index) {
		t.Fatalf("action = %+v, want id %d monitoring with a rollback naming %s",
			got, actionID, index)
	}
	if exists, valid := concIndex(t, pool, index); !exists || !valid {
		t.Fatalf("index %s exists=%v valid=%v, want a valid index", index, exists, valid)
	}
}

// TestCoverage_ExecuteManual_ConcurrentlyMissingTable verifies that a build
// on a missing table fails naming the table, is recorded failed and leaves
// the finding open for another attempt.
func TestCoverage_ExecuteManual_ConcurrentlyMissingTable(t *testing.T) {
	pool := concDB(t)
	table := fmt.Sprintf("conc_missing_%d", time.Now().UnixNano())
	index := table + "_id"
	sql := "CREATE INDEX CONCURRENTLY " + index + " ON public." + table + " (id)"
	findingID := concFinding(t, pool, table, sql)
	e := concExecutor(t, pool, 5000)

	actionID, err := e.ExecuteManual(context.Background(), findingID, sql, "", nil)
	if err == nil {
		t.Fatalf("ExecuteManual on a missing table = %d, nil; want an error", actionID)
	}
	if !strings.Contains(err.Error(), table) {
		t.Fatalf("error = %v, want it to name the table %s", err, table)
	}
	if got := oneAction(t, pool, findingID); got.outcome != "failed" || got.sql != sql {
		t.Fatalf("action = %+v, want one failed row for %q", got, sql)
	}
	if exists, _ := concIndex(t, pool, index); exists {
		t.Fatalf("index %s exists after a failed build", index)
	}
	if findingActed(t, pool, findingID) {
		t.Fatalf("finding %d marked acted on after a failed build", findingID)
	}
}

// TestCoverage_ExecuteManual_EmptySQLRefusedBeforeConnection verifies that
// empty SQL is refused before any database use: the executor has no pool,
// so any connection attempt would panic.
func TestCoverage_ExecuteManual_EmptySQLRefusedBeforeConnection(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	e := New(nil, cfg, time.Time{}, nopLog)
	for _, sql := range []string{"", "   ", "\n\t "} {
		actionID, err := e.ExecuteManual(context.Background(), 1, sql, "", nil)
		if !errors.Is(err, ErrDisallowedSQL) || !strings.Contains(err.Error(), "empty SQL") {
			t.Errorf("ExecuteManual(%q) = %d, %v; want ErrDisallowedSQL: empty SQL",
				sql, actionID, err)
		}
		if actionID != 0 {
			t.Errorf("ExecuteManual(%q) actionID = %d, want 0", sql, actionID)
		}
	}
}

// TestCoverage_ExecuteManual_ConcurrentlyLockTimeout verifies that a build
// held up by an older snapshot in its own database fails on the lock
// timeout instead of hanging: the error names the timeout, no valid index
// is left, and the failed action records its remnant cleanup.
func TestCoverage_ExecuteManual_ConcurrentlyLockTimeout(t *testing.T) {
	pool := concDB(t)
	table := concTable(t, pool)
	index := table + "_id"
	sql := "CREATE INDEX CONCURRENTLY " + index + " ON public." + table + " (id)"
	findingID := concFinding(t, pool, table, sql)
	e := concExecutor(t, pool, 1000)
	ctx := context.Background()
	holder, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin snapshot holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	// SELECT 1 takes the transaction snapshot without locking the table.
	if _, err := holder.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("take holder snapshot: %v", err)
	}

	start := time.Now()
	actionID, err := e.ExecuteManual(ctx, findingID, sql, "", nil)
	if err == nil {
		t.Fatalf("ExecuteManual behind an old snapshot = %d, nil; want a lock timeout", actionID)
	}
	if !errors.Is(err, ErrLockNotAvailable) || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("error = %v, want ErrLockNotAvailable naming the lock timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("ExecuteManual took %v behind a 1s lock timeout", elapsed)
	}
	if _, valid := concIndex(t, pool, index); valid {
		t.Fatalf("index %s is valid after a timed-out build", index)
	}
	got := oneAction(t, pool, findingID)
	if got.outcome != "failed" || got.sql != sql || !got.remnant {
		t.Fatalf("action = %+v, want one failed row with its remnant cleanup", got)
	}
}

// TestCoverage_ExecuteManual_ConcurrentlyTwoIndexesOneTable runs two
// operator builds of different indexes on one table at once. The operator
// lease lets one action hold a table at a time, so each call either
// succeeds or is refused with ErrTargetLeased (never a deadlock, timeout or
// other error); a refused call succeeds when repeated, and both indexes end
// valid with one action each.
func TestCoverage_ExecuteManual_ConcurrentlyTwoIndexesOneTable(t *testing.T) {
	pool := concDB(t)
	table := concTable(t, pool)
	indexes := []string{table + "_id", table + "_val"}
	cols := []string{"id", "val"}
	sqls := make([]string, 2)
	findings := make([]int, 2)
	for i := range indexes {
		sqls[i] = "CREATE INDEX CONCURRENTLY " + indexes[i] + " ON public." + table +
			" (" + cols[i] + ")"
		findings[i] = concFinding(t, pool, table, sqls[i])
	}
	e := concExecutor(t, pool, 5000)
	ids, errs := runConcurrently(e, findings, sqls)
	for i, err := range errs {
		if errors.Is(err, ErrTargetLeased) {
			ids[i], err = e.ExecuteManual(context.Background(), findings[i], sqls[i], "", nil)
		}
		if err != nil || ids[i] <= 0 {
			t.Fatalf("build %s = %d, %v; want an action id", indexes[i], ids[i], err)
		}
		if exists, valid := concIndex(t, pool, indexes[i]); !exists || !valid {
			t.Fatalf("index %s exists=%v valid=%v, want valid", indexes[i], exists, valid)
		}
		if got := oneAction(t, pool, findings[i]); got.id != ids[i] || got.sql != sqls[i] {
			t.Fatalf("action of %s = %+v, want id %d", indexes[i], got, ids[i])
		}
	}
	if ids[0] == ids[1] {
		t.Fatalf("both builds returned action id %d", ids[0])
	}
}

// runConcurrently starts one ExecuteManual per finding at the same moment.
func runConcurrently(e *Executor, findings []int, sqls []string) ([]int64, []error) {
	ids := make([]int64, len(findings))
	errs := make([]error, len(findings))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range findings {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = e.ExecuteManual(context.Background(), findings[i], sqls[i], "", nil)
		}(i)
	}
	close(start)
	wg.Wait()
	return ids, errs
}
