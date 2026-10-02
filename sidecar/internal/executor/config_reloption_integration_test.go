package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

func reloptionTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name, with string) string {
	t.Helper()
	ddl := "DROP TABLE IF EXISTS public." + name + "; CREATE TABLE public." + name +
		" (id bigint, v text)" + with
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+name)
	})
	return "public." + name
}

func reloptions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	var opts []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
		WHERE oid = to_regclass($1)`, table).Scan(&opts); err != nil {
		t.Fatalf("reloptions %s: %v", table, err)
	}
	return opts
}

func TestConfigRoundTrip_ReloptionPriorRestored(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := reloptionTable(t, ctx, pool, "p0_rel_prior", " WITH (fillfactor = 80)")
	sql := "ALTER TABLE " + table + " SET (fillfactor = 90)"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cc.rollbackSQL != "ALTER TABLE "+table+" SET (fillfactor = 80)" {
		t.Fatalf("rollback = %q, want the prior fillfactor 80", cc.rollbackSQL)
	}
	if err := ExecInTransaction(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	before := map[string]any{}
	cc.record(before)
	id := insertConfigAction(t, ctx, pool, sql, cc.rollbackSQL, before)
	if !e.settleConfigChange(ctx, id, cc) {
		t.Fatal("an in-effect reloption must proceed to the outcome monitor")
	}
	row := readAction(t, ctx, pool, id)
	if row.Outcome != "monitoring" || nested(row.After, "config_readback", "state") != "in_effect" {
		t.Fatalf("action = %+v, want monitoring with in_effect read-back", row)
	}
	if got := reloptions(t, ctx, pool, table); len(got) != 1 || got[0] != "fillfactor=90" {
		t.Fatalf("reloptions after apply = %v", got)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL, RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := reloptions(t, ctx, pool, table); len(got) != 1 || got[0] != "fillfactor=80" {
		t.Fatalf("reloptions after rollback = %v, want [fillfactor=80]", got)
	}
}

func TestConfigRoundTrip_ReloptionAbsentPriorReset(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := reloptionTable(t, ctx, pool, "p0_rel_absent", "")
	sql := "ALTER TABLE " + table + " SET (autovacuum_vacuum_scale_factor = 0.02)"
	cc, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := "ALTER TABLE " + table + " RESET (autovacuum_vacuum_scale_factor)"
	if cc.rollbackSQL != want {
		t.Fatalf("rollback = %q, want %q", cc.rollbackSQL, want)
	}
	if err := ExecInTransaction(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := reloptions(t, ctx, pool, table); len(got) != 1 {
		t.Fatalf("reloptions after apply = %v", got)
	}
	if _, err := executeRollbackSQL(ctx, pool, cc.rollbackSQL, RollbackMonitorConfig{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := reloptions(t, ctx, pool, table); len(got) != 0 {
		t.Fatalf("reloptions after rollback = %v, want none", got)
	}
}

// A disallowed reloption never reaches the table, on the autonomous path
// (recorded as a failed action) and on the operator path (refused).
func TestConfigApply_DisallowedReloptionRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	table := reloptionTable(t, ctx, pool, "p0_rel_refused", "")
	sql := "ALTER TABLE " + table + " SET (autovacuum_enabled = false)"
	e.executeFinding(ctx, analyzer.Finding{Category: "vacuum_tuning",
		ObjectIdentifier: table, Title: "disable autovacuum", RecommendedSQL: sql,
		ActionRisk: "safe"}, 0, ActionPolicyDecision{})
	var outcome, reason string
	err := pool.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason,'')
		FROM sage.action_log WHERE sql_executed=$1 ORDER BY id DESC LIMIT 1`, sql).
		Scan(&outcome, &reason)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE sql_executed=$1",
			sql)
	})
	if err != nil || outcome != "failed" || !strings.Contains(reason, "autovacuum_enabled") {
		t.Fatalf("action = %q/%q (%v), want failed naming autovacuum_enabled", outcome, reason, err)
	}
	if _, err := e.ExecuteManual(ctx, 1, sql, "", nil); !errors.Is(err, ErrDisallowedSQL) {
		t.Fatalf("ExecuteManual = %v, want ErrDisallowedSQL", err)
	}
	if got := reloptions(t, ctx, pool, table); len(got) != 0 {
		t.Fatalf("reloptions = %v, the refused option reached the table", got)
	}
}

// An unknown GUC is never executable, whatever path asks.
func TestConfigApply_UnknownGUCRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	e := configTestExecutor(pool)
	_, err := e.ExecuteManual(ctx, 1, "ALTER SYSTEM SET log_statement = 'all'", "", nil)
	if !errors.Is(err, ErrDisallowedSQL) {
		t.Fatalf("ExecuteManual(log_statement) = %v, want ErrDisallowedSQL", err)
	}
}

// The approval path (ExecuteManual) used to write postgresql.auto.conf
// without a reload or a captured rollback: the change was neither in
// effect nor reversible.
func TestConfigManualPath_ReloadsAndCapturesRollback(t *testing.T) {
	ctx := context.Background()
	pool := lockAlterSystem(t, ctx, "work_mem")
	const sql = "ALTER SYSTEM SET work_mem = '28MB'"
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql)
		VALUES ('memory_tuning','warning','configuration','instance:p0_manual',
		        'p0 manual','{}','rec',$1) RETURNING id`, sql).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE finding_id=$1", findingID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id=$1", findingID)
	})
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Trust.RollbackWindowMinutes = 0
	e := New(pool, cfg, zeroTime(), nopLog)
	e.emergencyStopFn = func(context.Context) bool { return false }
	withTestStandingGate(e)
	if _, err := e.ExecuteManual(ctx, findingID, sql, "ALTER SYSTEM SET work_mem = '1MB'",
		nil); err != nil {
		t.Fatalf("ExecuteManual: %v", err)
	}
	if got := mustSetting(t, ctx, pool, "work_mem"); got.Setting != "28672" {
		t.Fatalf("work_mem = %s after an approved change, want 28672 in effect", got.Setting)
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM sage.action_log WHERE finding_id=$1
		ORDER BY id DESC LIMIT 1`, findingID).Scan(&id); err != nil {
		t.Fatalf("action row: %v", err)
	}
	row := waitOutcome(t, ctx, pool, id)
	if row.Rollback != "ALTER SYSTEM RESET work_mem" || row.Outcome == "success" {
		t.Fatalf("manual action = %+v, want captured RESET and no blind success", row)
	}
}

// The outcome monitor credits a change only when its targeted metric
// moved: here the real temp_files counter against a recorded baseline.
func TestConfigOutcome_TempSpillsMeasuredAgainstBaseline(t *testing.T) {
	pool, ctx := requireDB(t)
	var tempFiles float64
	if err := pool.QueryRow(ctx, `SELECT temp_files FROM pg_stat_database
		WHERE datname = current_database()`).Scan(&tempFiles); err != nil {
		t.Fatalf("temp_files: %v", err)
	}
	cases := []struct {
		rate float64
		want string
	}{
		{1, "success"},     // 600 spills expected in 10 min, none seen
		{0, "unverifiable"}, // nothing spilled before: nothing to improve
	}
	for _, c := range cases {
		before := map[string]any{
			"cache_hit_ratio": 0.5,
			"config_change": map[string]any{"kind": "guc", "name": "work_mem",
				"outcome": map[string]any{"metric": metricTempSpills,
					"at": time.Now().Add(-10 * time.Minute).Format(time.RFC3339Nano),
					"counters": map[string]any{"temp_files": tempFiles,
						"rate_per_sec": c.rate}}},
		}
		id := insertConfigAction(t, ctx, pool, "ALTER SYSTEM SET work_mem = '64MB'",
			"ALTER SYSTEM RESET work_mem", before)
		MonitorAndRollback(ctx, pool, id, "ALTER SYSTEM RESET work_mem",
			RollbackMonitorConfig{ThresholdPct: 100}, nopLog, nil)
		row := readAction(t, ctx, pool, id)
		if row.Outcome != c.want {
			t.Errorf("rate %v: outcome = %q (%s), want %q", c.rate, row.Outcome, row.Reason, c.want)
		}
		if c.want == "success" && nested(row.After, "config_outcome", "metric") != metricTempSpills {
			t.Errorf("success without the measured metric in after_state: %v", row.After)
		}
	}
}

// A config action with no outcome baseline is never credited.
func TestConfigOutcome_NoBaselineIsUnverifiable(t *testing.T) {
	pool, ctx := requireDB(t)
	before := map[string]any{"cache_hit_ratio": 0.5,
		"config_change": map[string]any{"kind": "guc", "name": "random_page_cost"}}
	id := insertConfigAction(t, ctx, pool, "ALTER SYSTEM SET random_page_cost = 1.1",
		"ALTER SYSTEM RESET random_page_cost", before)
	MonitorAndRollback(ctx, pool, id, "ALTER SYSTEM RESET random_page_cost",
		RollbackMonitorConfig{ThresholdPct: 100}, nopLog, nil)
	row := readAction(t, ctx, pool, id)
	if row.Outcome != "unverifiable" || !strings.Contains(row.Reason, "no targeted metric") {
		t.Fatalf("outcome = %q (%s), want unverifiable / no targeted metric", row.Outcome,
			row.Reason)
	}
}

// Baseline capture reads real counters for each metric kind.
func TestCaptureOutcomeBaseline_Live(t *testing.T) {
	pool, ctx := requireDB(t)
	table := reloptionTable(t, ctx, pool, "p0_baseline", "")
	if _, err := pool.Exec(ctx, "INSERT INTO "+table+
		" SELECT g, 'x' FROM generate_series(1, 100) g"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, c := range []struct{ metric, table, counter string }{
		{metricTempSpills, "", "temp_files"},
		{metricDeadTuples, table, "autovacuum_count"},
		{metricDeadTuples, "", "n_live_tup"},
		{metricHotUpdates, table, "n_tup_upd"},
	} {
		b, err := captureOutcomeBaseline(ctx, pool, c.metric, c.table)
		if err != nil || b == nil {
			t.Fatalf("%s/%s: %v", c.metric, c.table, err)
		}
		if _, ok := b.Counters[c.counter]; !ok || b.At.IsZero() {
			t.Errorf("%s/%s baseline = %+v, missing %s", c.metric, c.table, b, c.counter)
		}
	}
	if _, err := captureOutcomeBaseline(ctx, pool, metricHotUpdates,
		"public.p0_missing_table"); err == nil {
		t.Error("baseline of a missing table succeeded")
	}
}
