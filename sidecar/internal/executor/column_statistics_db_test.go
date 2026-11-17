package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// review-02c on a real server (PG14 and PG17): the forward ALTER, its
// read-back, the rollback to the prior target, the lock_timeout failure
// and the change lease that serializes two actions on one table.

func columnStatsTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) string {
	t.Helper()
	ddl := "DROP TABLE IF EXISTS public." + name + "; CREATE TABLE public." + name +
		" (id bigint, deleted_at timestamptz, status text)"
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+name)
	})
	return "public." + name
}

// statTarget is pg_attribute.attstattarget (nil: NULL, the PG17 default).
func statTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table, column string) *int16 {
	t.Helper()
	var v *int16
	if err := pool.QueryRow(ctx, `SELECT attstattarget::int2 FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND attname = $2`, table, column).Scan(&v); err != nil {
		t.Fatalf("attstattarget %s.%s: %v", table, column, err)
	}
	return v
}

func isDefaultTarget(v *int16) bool { return v == nil || *v == -1 }

// Forward, read-back and verify, then rollback to the default (-1 before
// PG17, NULL on PG17+).
func TestColumnStatistics_DBRoundTripToDefault(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := columnStatsTable(t, ctx, pool, "r02c_roundtrip")
	if !isDefaultTarget(statTarget(t, ctx, pool, table, "deleted_at")) {
		t.Fatal("a fresh column must have the default statistics target")
	}
	sql := "ALTER TABLE " + table + " ALTER COLUMN deleted_at SET STATISTICS 1000"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil || cc == nil {
		t.Fatalf("prepare = %v, %v", cc, err)
	}
	want := "ALTER TABLE " + table + " ALTER COLUMN deleted_at SET STATISTICS -1"
	if cc.kind != configKindColumnStatistics || cc.rollbackSQL != want {
		t.Fatalf("prepared kind %q rollback %q, want %q / %q", cc.kind, cc.rollbackSQL,
			configKindColumnStatistics, want)
	}
	if err := ExecInTransaction(ctx, pool, sql, time.Minute, WithLockTimeout(2000)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before := map[string]any{}
	cc.record(before)
	if nested(before, "config_change", "kind") != configKindColumnStatistics {
		t.Fatalf("before_state = %v, want the column statistics prior recorded", before)
	}
	id := insertConfigAction(t, ctx, pool, sql, cc.rollbackSQL, before)
	if !e.settleConfigChange(ctx, id, cc) {
		t.Fatal("an in-effect statistics target must proceed to the outcome monitor")
	}
	row := readAction(t, ctx, pool, id)
	if nested(row.After, "config_readback", "state") != string(readbackInEffect) {
		t.Fatalf("read-back = %v, want in_effect", row.After)
	}
	if got := statTarget(t, ctx, pool, table, "deleted_at"); got == nil || *got != 1000 {
		t.Fatalf("attstattarget after apply = %v, want 1000", got)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL,
		RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := statTarget(t, ctx, pool, table, "deleted_at"); !isDefaultTarget(got) {
		t.Fatalf("attstattarget after rollback = %v, want the default", *got)
	}
}

func TestColumnStatistics_DBPriorTargetRestored(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := columnStatsTable(t, ctx, pool, "r02c_prior")
	if _, err := pool.Exec(ctx, "ALTER TABLE "+table+
		" ALTER COLUMN status SET STATISTICS 250"); err != nil {
		t.Fatal(err)
	}
	sql := "ALTER TABLE " + table + " ALTER COLUMN status SET STATISTICS 2500"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if want := "ALTER TABLE " + table + " ALTER COLUMN status SET STATISTICS 250"; cc.
		rollbackSQL != want {
		t.Fatalf("rollback = %q, want %q", cc.rollbackSQL, want)
	}
	if err := ExecInTransaction(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL,
		RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := statTarget(t, ctx, pool, table, "status"); got == nil || *got != 250 {
		t.Fatalf("attstattarget after rollback = %v, want 250", got)
	}
}

// A prior of 0 (statistics switched off by the owner) is a value the
// contract never sets, so no faithful rollback exists: refused, untouched.
func TestColumnStatistics_DBPriorZeroRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := columnStatsTable(t, ctx, pool, "r02c_zero")
	if _, err := pool.Exec(ctx, "ALTER TABLE "+table+
		" ALTER COLUMN status SET STATISTICS 0"); err != nil {
		t.Fatal(err)
	}
	sql := "ALTER TABLE " + table + " ALTER COLUMN status SET STATISTICS 1000"
	if _, err := e.prepareConfigChange(ctx, sql); !errors.Is(err, ErrNoFaithfulRollback) {
		t.Fatalf("prepare with prior 0 = %v, want ErrNoFaithfulRollback", err)
	}
	missing := "ALTER TABLE " + table + " ALTER COLUMN nope SET STATISTICS 1000"
	if _, err := e.prepareConfigChange(ctx, missing); !errors.Is(err, ErrConfigPrior) ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("prepare of a missing column = %v, want ErrConfigPrior naming it", err)
	}
}

// A SET STATISTICS that cannot get its lock fails with the lock_timeout
// error; nothing changed, so no rollback runs.
func TestColumnStatistics_DBLockTimeoutNoRollback(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	cfg := *e.cfg
	cfg.Safety.LockTimeoutMs = 300
	e.cfg = &cfg
	table := columnStatsTable(t, ctx, pool, "r02c_locked")
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	sql := "ALTER TABLE " + table + " ALTER COLUMN deleted_at SET STATISTICS 1000"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE sql_executed=$1",
			sql)
	})
	e.executeFinding(ctx, analyzer.Finding{Category: "plan_stability",
		ObjectIdentifier: table, Title: "statistics target", RecommendedSQL: sql,
		ActionRisk: "moderate"}, 0, ActionPolicyDecision{})
	_ = blocker.Rollback(ctx)
	var outcome, reason string
	var actions int
	err = pool.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason,''),
		count(*) OVER () FROM sage.action_log WHERE sql_executed=$1
		ORDER BY id DESC LIMIT 1`, sql).Scan(&outcome, &reason, &actions)
	if err != nil {
		t.Fatalf("read action: %v", err)
	}
	// "failed", never "rolled_back": nothing changed, so no rollback ran.
	if outcome != "failed" || !strings.Contains(reason, "lock timeout") || actions != 1 {
		t.Fatalf("action = %q/%q (%d rows), want one failed row naming the lock timeout",
			outcome, reason, actions)
	}
	if got := statTarget(t, ctx, pool, table, "deleted_at"); !isDefaultTarget(got) {
		t.Fatalf("attstattarget = %d after a lock timeout, want untouched", *got)
	}
}

// Two set_column_statistics actions on one table: the second is refused
// while the first holds the table's change lease (park mode).
func TestColumnStatistics_DBSecondOnSameTableRefused(t *testing.T) {
	first := "ALTER TABLE public.{table} ALTER COLUMN a SET STATISTICS 1000"
	pool, table, findingID := manualFixture(t, first)
	first = strings.ReplaceAll(first, "{table}", table)
	exec := withSerializeGate(manualExecutor(pool), policy.SerializePark)
	secondSQL := "ALTER TABLE public." + table + " ALTER COLUMN b SET STATISTICS 2000"
	second := secondOperatorFinding(t, pool, table, secondSQL)
	unblock, firstDone := startBlockedOperator(t, exec, pool, table, findingID, first)

	_, err := exec.ExecuteManual(context.Background(), second, secondSQL, "", nil)
	unblock()

	if !errors.Is(err, ErrTargetLeased) {
		t.Fatalf("second statistics action = %v, want ErrTargetLeased", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first statistics action = %v, want success", err)
	}
	if n := findingActions(t, pool, second); n != 0 {
		t.Fatalf("refused second action wrote %d rows, want 0", n)
	}
	ctx := context.Background()
	if got := statTarget(t, ctx, pool, "public."+table, "a"); got == nil || *got != 1000 {
		t.Fatalf("a = %v, want the first action's 1000", got)
	}
	if got := statTarget(t, ctx, pool, "public."+table, "b"); !isDefaultTarget(got) {
		t.Fatalf("b = %d, want untouched by the refused action", *got)
	}
}

// The Clerk shape: 1,000,000 rows, 4 non-null deleted_at. The default
// sample misses them; the raised target sees them, and the planner's
// estimate for IS NOT NULL changes after ANALYZE.
func TestColumnStatistics_DBExplainEstimateChangesAfterAnalyze(t *testing.T) {
	pool, ctx := requireDB(t)
	table := columnStatsTable(t, ctx, pool, "r02c_clerk")
	fill := fmt.Sprintf(`INSERT INTO %s (id, deleted_at, status)
		SELECT g, CASE WHEN g %% 250000 = 0 THEN now() END, 'live'
		FROM generate_series(1, 1000000) g`, table)
	if _, err := pool.Exec(ctx, fill); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE "+table); err != nil {
		t.Fatal(err)
	}
	before := planRows(t, ctx, pool, table)
	sql := "ALTER TABLE " + table + " ALTER COLUMN deleted_at SET STATISTICS 10000"
	if err := ExecInTransaction(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE "+table); err != nil {
		t.Fatal(err)
	}
	after := planRows(t, ctx, pool, table)
	if after == before {
		t.Fatalf("estimate for IS NOT NULL stayed %v rows after the raised target", before)
	}
	if after < 2 || after > 50 {
		t.Fatalf("estimate after = %v rows, want close to the 4 real rows", after)
	}
}

func planRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) float64 {
	t.Helper()
	var plan []map[string]map[string]any
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) SELECT id FROM "+table+
		" WHERE deleted_at IS NOT NULL").Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	rows, _ := plan[0]["Plan"]["Plan Rows"].(float64)
	return rows
}

// Without ONLY, PostgreSQL sets the target on every inheritance child too.
// A child with its own target could not get it back from the rollback, so
// the change is refused; children that share the parent's target are
// restored with it; with ONLY the child is never touched (review-02c
// review).
func TestColumnStatistics_DBInheritanceChildren(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	parent := columnStatsTable(t, ctx, pool, "r02c_parent")
	child := "public.r02c_child"
	if _, err := pool.Exec(ctx, "CREATE TABLE "+child+" () INHERITS ("+parent+")"); err != nil {
		t.Fatalf("create child: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+child) })
	sql := "ALTER TABLE " + parent + " ALTER COLUMN status SET STATISTICS 1000"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil || cc == nil {
		t.Fatalf("prepare with a child on the default = %v, %v; want accepted", cc, err)
	}
	if err := ExecInTransaction(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL,
		RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := statTarget(t, ctx, pool, child, "status"); !isDefaultTarget(got) {
		t.Fatalf("child target after rollback = %v, want the default", statTargetText(got))
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE ONLY "+child+
		" ALTER COLUMN status SET STATISTICS 50"); err != nil {
		t.Fatal(err)
	}
	_, err = e.prepareConfigChange(ctx, sql)
	if !errors.Is(err, ErrNoFaithfulRollback) || !strings.Contains(err.Error(), "r02c_child") {
		t.Fatalf("prepare with a child at 50 = %v, want ErrNoFaithfulRollback naming it", err)
	}
	only := "ALTER TABLE ONLY " + parent + " ALTER COLUMN status SET STATISTICS 1000"
	cc, err = e.prepareConfigChange(ctx, only)
	if err != nil || !strings.HasPrefix(cc.rollbackSQL, "ALTER TABLE ONLY ") {
		t.Fatalf("prepare ONLY = %v, %v; want a rollback with ONLY", cc, err)
	}
	if err := ExecInTransaction(ctx, pool, only, time.Minute); err != nil {
		t.Fatalf("apply ONLY: %v", err)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL,
		RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback ONLY: %v", err)
	}
	if got := statTarget(t, ctx, pool, child, "status"); got == nil || *got != 50 {
		t.Fatalf("child target after ONLY round trip = %v, want 50", statTargetText(got))
	}
}
