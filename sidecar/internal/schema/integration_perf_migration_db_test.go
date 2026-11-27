package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The perf gate step on a database: built once, left alone on a second
// boot, the retired updated_at indexes dropped on an install that has
// them, and the plans it exists for. No concurrent-writer test: Bootstrap
// holds its advisory lock, so one migration runs per database at a time.

func indexOID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) uint32 {
	t.Helper()
	var oid *uint32
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1)::oid`, name).
		Scan(&oid); err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	if oid == nil {
		return 0
	}
	return *oid
}

func bootstrapPerfPool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := freshSchemaPool(t, label)
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func TestIntegrationPerfMigration_BootstrapTwiceKeepsTheIndexes(t *testing.T) {
	pool, ctx := bootstrapPerfPool(t, "integrationperf")
	names := []string{"idx_action_log_last_failure", "idx_workload_role_daily_day",
		"idx_budget_shadow_day"}
	first := map[string]uint32{}
	for _, name := range names {
		if first[name] = indexOID(t, ctx, pool, name); first[name] == 0 {
			t.Fatalf("index %s missing after bootstrap", name)
		}
	}
	for _, name := range retiredUpdatedAtIndexes {
		if indexOID(t, ctx, pool, name) != 0 {
			t.Fatalf("retired index %s still present after bootstrap", name)
		}
	}
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_indexdef('sage.idx_action_log_last_failure'::regclass)`).
		Scan(&def); err != nil {
		t.Fatalf("read gauge index: %v", err)
	}
	if !strings.Contains(def, "(COALESCE(measured_at, executed_at))") ||
		!strings.Contains(def, "'failed'::text, 'partial'::text, 'rollback_failed'::text") {
		t.Fatalf("gauge index = %s", def)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	for _, name := range names {
		if again := indexOID(t, ctx, pool, name); again != first[name] {
			t.Fatalf("index %s rebuilt on the second boot (%d -> %d)", name, first[name], again)
		}
	}
}

// An install from before the step has the updated_at indexes: the step
// drops them and builds the day index, and a second run is a no-op.
func TestIntegrationPerfMigration_DropsTheRetiredIndexes(t *testing.T) {
	pool, ctx := bootstrapPerfPool(t, "integrationperfdrop")
	if _, err := pool.Exec(ctx, `
		CREATE INDEX workload_role_daily_updated_at_idx ON sage.workload_role_daily (updated_at);
		CREATE INDEX budget_shadow_updated_at_idx ON sage.budget_shadow (updated_at);
		DROP INDEX sage.idx_workload_role_daily_day;`); err != nil {
		t.Fatalf("restore the pre-step indexes: %v", err)
	}
	var built uint32
	for run := range 2 {
		if _, err := pool.Exec(ctx, ddlIntegrationPerfIndexes()); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
		for _, name := range retiredUpdatedAtIndexes {
			if indexOID(t, ctx, pool, name) != 0 {
				t.Fatalf("run %d: retired index %s still present", run, name)
			}
		}
		oid := indexOID(t, ctx, pool, "idx_workload_role_daily_day")
		if oid == 0 || (run == 1 && oid != built) {
			t.Fatalf("run %d: idx_workload_role_daily_day oid %d (first run %d)", run, oid, built)
		}
		built = oid
	}
}

const perfRoleUpsert = `INSERT INTO sage.workload_role_daily (database_name, role_name, day,
	p99_estimate_ms, max_exec_ms, calls) VALUES ('db', 'app', current_date, $1, $1, $2)
	ON CONFLICT (database_name, role_name, day) DO UPDATE SET
	p99_estimate_ms = GREATEST(sage.workload_role_daily.p99_estimate_ms,
	                           EXCLUDED.p99_estimate_ms),
	max_exec_ms = GREATEST(sage.workload_role_daily.max_exec_ms, EXCLUDED.max_exec_ms),
	calls = GREATEST(sage.workload_role_daily.calls, EXCLUDED.calls),
	updated_at = now()`

// The budget store's role upsert moves only unindexed columns now: every
// update of the day's row is HOT (perf gate F had 0 of 7).
func TestIntegrationPerfMigration_RoleUpsertsAreHOT(t *testing.T) {
	pool, ctx := bootstrapPerfPool(t, "integrationperfhot")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := range 6 {
		if _, err := tx.Exec(ctx, perfRoleUpsert, float64(10+i), int64(100+i)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	var upd, hot int64
	if err := tx.QueryRow(ctx, `SELECT n_tup_upd, n_tup_hot_upd
		FROM pg_stat_xact_user_tables
		WHERE schemaname = 'sage' AND relname = 'workload_role_daily'`).
		Scan(&upd, &hot); err != nil {
		t.Fatalf("read update counters: %v", err)
	}
	if upd != 5 || hot != 5 {
		t.Fatalf("role upserts: %d updates, %d HOT; want 5 and 5", upd, hot)
	}
	var calls int64
	if err := tx.QueryRow(ctx, `SELECT calls FROM sage.workload_role_daily
		WHERE database_name = 'db' AND role_name = 'app'`).Scan(&calls); err != nil ||
		calls != 105 {
		t.Fatalf("calls = %d (%v), want 105", calls, err)
	}
}

func explainLines(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (COSTS OFF) "+sql)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// The executor's last-failure gauge reads one index entry, not the
// ledger, and returns what a full read returns.
func TestIntegrationPerfMigration_LastFailureGaugeUsesTheIndex(t *testing.T) {
	pool, ctx := bootstrapPerfPool(t, "integrationperfgauge")
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, outcome, measured_at)
		SELECT now() - g * interval '1 minute', 'vacuum', 'SELECT 1',
		       (ARRAY['success', 'failed', 'rolled_back', 'partial'])[1 + g % 4],
		       CASE WHEN g % 8 = 1 THEN now() - g * interval '1 minute' + interval '2 hours' END
		  FROM generate_series(1, 4000) g`); err != nil {
		t.Fatalf("seed the ledger: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE sage.action_log`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	gauge := `SELECT max(COALESCE(measured_at, executed_at))::text FROM sage.action_log
		WHERE outcome IN ('failed', 'partial', 'rollback_failed')`
	plan := explainLines(t, ctx, pool, gauge)
	if !strings.Contains(plan, "idx_action_log_last_failure") || strings.Contains(plan, "Seq Scan") {
		t.Fatalf("gauge plan does not use idx_action_log_last_failure:\n%s", plan)
	}
	var got, want string
	if err := pool.QueryRow(ctx, gauge).Scan(&got); err != nil {
		t.Fatalf("gauge: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT max(COALESCE(measured_at, executed_at))::text
		FROM (SELECT * FROM sage.action_log OFFSET 0) l
		WHERE outcome IN ('failed', 'partial', 'rollback_failed')`).Scan(&want); err != nil {
		t.Fatalf("reference: %v", err)
	}
	if got == "" || got != want {
		t.Fatalf("gauge = %q, want %q", got, want)
	}
}
