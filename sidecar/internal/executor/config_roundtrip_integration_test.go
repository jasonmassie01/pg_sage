package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Live-PostgreSQL round trips for config changes (G-P0-1): prior value
// captured at apply time, real rollback SQL, read-back after reload, and
// an outcome check that never credits an unmeasured change. ALTER SYSTEM
// is cluster-wide, so every test holds the cluster lock and restores the
// settings it touched.

func lockAlterSystem(t *testing.T, ctx context.Context, gucs ...string) *pgxpool.Pool {
	t.Helper()
	pool, _ := requireDB(t)
	release, err := testdb.LockCluster(ctx, testDSN(), "alter_system")
	if err != nil {
		t.Fatalf("cluster lock: %v", err)
	}
	reset := func() {
		for _, name := range gucs {
			_, _ = pool.Exec(context.Background(), "ALTER SYSTEM RESET "+name)
		}
		_, _ = pool.Exec(context.Background(), "SELECT pg_reload_conf()")
	}
	reset()
	t.Cleanup(func() {
		reset()
		release()
	})
	return pool
}

func configTestExecutor(pool *pgxpool.Pool) *Executor {
	cfg := config.DefaultConfig()
	cfg.Trust.RollbackWindowMinutes = 0
	e := New(pool, cfg, zeroTime(), nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	e.settingWait = 3 * time.Second
	return e
}

func mustSetting(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) settingRow {
	t.Helper()
	row, err := readSettingRow(ctx, pool, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return row
}

// insertConfigAction records an action_log row the settle step updates.
func insertConfigAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sql, rollback string, before map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(before)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, before_state, outcome)
		VALUES ('alter', $1, NULLIF($2,''), $3, 'monitoring') RETURNING id`,
		sql, rollback, raw).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

type actionRow struct {
	Outcome, Reason, Rollback string
	Before, After             map[string]any
}

func readAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) actionRow {
	t.Helper()
	var r actionRow
	var before, after []byte
	if err := pool.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason,''),
		COALESCE(rollback_sql,''), COALESCE(before_state,'{}'), COALESCE(after_state,'{}')
		FROM sage.action_log WHERE id=$1`, id).Scan(
		&r.Outcome, &r.Reason, &r.Rollback, &before, &after); err != nil {
		t.Fatalf("read action %d: %v", id, err)
	}
	_ = json.Unmarshal(before, &r.Before)
	_ = json.Unmarshal(after, &r.After)
	return r
}

func waitOutcome(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) actionRow {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r := readAction(t, ctx, pool, id)
		if r.Outcome != "monitoring" || time.Now().After(deadline) {
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func nested(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		next, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = next[k]
	}
	return cur
}

func TestConfigRoundTrip_ReloadGUC(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem")
	e := configTestExecutor(pool)
	prior := mustSetting(t, ctx, pool, "work_mem")
	const sql = "ALTER SYSTEM SET work_mem = '24MB'"

	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil || cc == nil {
		t.Fatalf("prepareConfigChange = %+v, %v", cc, err)
	}
	if cc.rollbackSQL != "ALTER SYSTEM RESET work_mem" ||
		cc.priorGUC.Setting != prior.Setting || cc.priorGUC.Source != "default" {
		t.Fatalf("prior/rollback = %+v / %q, want default %s / RESET", cc.priorGUC,
			cc.rollbackSQL, prior.Setting)
	}
	if err := ExecConcurrently(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	out := applyConfigChange(ctx, pool, sql, "", nil)
	if !out.InEffect || out.PendingRestart || out.Effective != "24576" {
		t.Fatalf("read-back = %+v, want in effect at 24576kB", out)
	}
	if got := mustSetting(t, ctx, pool, "work_mem"); got.Setting != "24576" {
		t.Fatalf("pg_settings work_mem = %s after apply", got.Setting)
	}
	note, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL, RollbackMonitorConfig{})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after := mustSetting(t, ctx, pool, "work_mem")
	if after.Setting != prior.Setting || after.Source != "default" {
		t.Fatalf("after rollback = %+v (note %q), want %s from default", after, note,
			prior.Setting)
	}
}

func TestConfigRoundTrip_AutoConfPriorRestoredExactly(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem")
	if _, err := pool.Exec(ctx, "ALTER SYSTEM SET work_mem = '6MB'"); err != nil {
		t.Fatalf("seed prior: %v", err)
	}
	seed := "ALTER SYSTEM SET work_mem = '6MB'"
	if out := applyConfigChange(ctx, pool, seed, "", nil); !out.InEffect {
		t.Fatalf("seed prior not in effect: %+v", out)
	}
	e := configTestExecutor(pool)
	cc, err := e.prepareConfigChange(ctx, "ALTER SYSTEM SET work_mem = '12MB'")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cc.rollbackSQL != "ALTER SYSTEM SET work_mem = '6144'" {
		t.Fatalf("rollback = %q, want the auto.conf prior 6144kB", cc.rollbackSQL)
	}
	if err := ExecConcurrently(ctx, pool, cc.sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	applyConfigChange(ctx, pool, cc.sql, "", nil)
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL, RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := mustSetting(t, ctx, pool, "work_mem"); got.Setting != "6144" {
		t.Fatalf("work_mem after rollback = %s, want 6144", got.Setting)
	}
}

func TestConfigRoundTrip_RestartGUCPendingThenRolledBack(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "wal_buffers")
	e := configTestExecutor(pool)
	const sql = "ALTER SYSTEM SET wal_buffers = '1MB'"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cc.rollbackSQL != "ALTER SYSTEM RESET wal_buffers" {
		t.Fatalf("rollback = %q, want RESET (prior source %s)", cc.rollbackSQL,
			cc.priorGUC.Source)
	}
	if err := ExecConcurrently(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before := map[string]any{}
	cc.record(before)
	id := insertConfigAction(t, ctx, pool, sql, cc.rollbackSQL, before)
	if monitor := e.settleConfigChange(ctx, id, cc); monitor {
		t.Fatal("a pending-restart change must not enter the success monitor")
	}
	row := readAction(t, ctx, pool, id)
	if row.Outcome != "applied_pending_restart" ||
		nested(row.After, "config_readback", "pending_restart") != true {
		t.Fatalf("action = %+v, want applied_pending_restart with pending_restart", row)
	}
	if !strings.Contains(row.Reason, "restart") {
		t.Errorf("reason %q does not say a restart is needed", row.Reason)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL, RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mustSetting(t, ctx, pool, "wal_buffers").PendingRestart {
		if time.Now().After(deadline) {
			t.Fatal("wal_buffers still pending restart after rollback")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConfigRoundTrip_NotInEffectIsRevertedAndFailed(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem")
	e := configTestExecutor(pool)
	e.settingWait = 500 * time.Millisecond
	cc, err := e.prepareConfigChange(ctx, "ALTER SYSTEM SET work_mem = '8MB'")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The server ends up with a different value than requested (as when a
	// later statement or an override wins): read-back must not match.
	if err := ExecConcurrently(ctx, pool, "ALTER SYSTEM SET work_mem = '16MB'",
		time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before := map[string]any{}
	cc.record(before)
	id := insertConfigAction(t, ctx, pool, cc.sql, cc.rollbackSQL, before)
	if e.settleConfigChange(ctx, id, cc) {
		t.Fatal("a change that did not take effect entered the success monitor")
	}
	row := readAction(t, ctx, pool, id)
	if row.Outcome != "failed" || !strings.Contains(row.Reason, "did not take effect") {
		t.Fatalf("action = %+v, want failed / did not take effect", row)
	}
	if got := mustSetting(t, ctx, pool, "work_mem"); got.Source != "default" {
		t.Fatalf("work_mem after revert = %+v, want the default restored", got)
	}
}

func TestConfigPrepare_DatabaseOverrideRefused(t *testing.T) {
	ctx := context.Background()
	base, _ := requireDB(t)
	var db string
	if err := base.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	quoted := `"` + strings.ReplaceAll(db, `"`, `""`) + `"`
	if _, err := base.Exec(ctx, "ALTER DATABASE "+quoted+" SET work_mem = '8MB'"); err != nil {
		t.Fatalf("database override: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "ALTER DATABASE "+quoted+" RESET work_mem")
	})
	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatalf("fresh pool: %v", err)
	}
	defer pool.Close()
	e := configTestExecutor(pool)
	cc, err := e.prepareConfigChange(ctx, "ALTER SYSTEM SET work_mem = '64MB'")
	if !errors.Is(err, ErrConfigOverridden) || cc != nil {
		t.Fatalf("prepare under a database override = %+v, %v; want ErrConfigOverridden", cc, err)
	}
}

func TestConfigPrepare_NonConfigSQLPassesThrough(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	for _, sql := range []string{"VACUUM public.x", "CREATE INDEX CONCURRENTLY i ON t (a)", ""} {
		cc, err := e.prepareConfigChange(ctx, sql)
		if cc != nil || err != nil {
			t.Errorf("prepareConfigChange(%q) = %+v, %v; want nil, nil", sql, cc, err)
		}
	}
	if _, err := e.prepareConfigChange(ctx,
		"ALTER TABLE public.no_such_table_p0 SET (fillfactor = 90)"); !errors.Is(err, ErrConfigPrior) {
		t.Errorf("missing table prior = %v, want ErrConfigPrior", err)
	}
}

// Through the real Apply pipeline: the action carries the captured
// rollback and read-back, and the monitor never records 'success' for a
// change whose targeted metric cannot be measured in the window.
func TestConfigApplyPath_GUCRecordedAndNeverBlindSuccess(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem")
	e := configTestExecutor(pool)
	const sql = "ALTER SYSTEM SET work_mem = '20MB'"
	f := analyzer.Finding{Category: "memory_tuning", ObjectType: "configuration",
		ObjectIdentifier: "instance:p0_apply_path", Title: "p0 apply path",
		RecommendedSQL: sql, RollbackSQL: "ALTER SYSTEM SET work_mem = '1MB'",
		ActionRisk: "moderate"}
	e.executeFinding(ctx, f, 0, ActionPolicyDecision{})
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.action_log WHERE sql_executed=$1
		ORDER BY id DESC LIMIT 1`, sql).Scan(&id); err != nil {
		t.Fatalf("no action row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	row := waitOutcome(t, ctx, pool, id)
	if row.Rollback != "ALTER SYSTEM RESET work_mem" {
		t.Errorf("rollback_sql = %q, want the captured RESET (not the LLM's guess)", row.Rollback)
	}
	if nested(row.Before, "config_change", "prior", "source") != "default" {
		t.Errorf("before_state.config_change = %v", row.Before["config_change"])
	}
	if nested(row.After, "config_readback", "state") != "in_effect" {
		t.Errorf("after_state.config_readback = %v", row.After["config_readback"])
	}
	if row.Outcome != "unverifiable" {
		t.Fatalf("outcome = %q (%s), want unverifiable", row.Outcome, row.Reason)
	}
	if got := mustSetting(t, ctx, pool, "work_mem"); got.Setting != "20480" {
		t.Errorf("work_mem = %s, want the applied 20480kB in effect", got.Setting)
	}
}
