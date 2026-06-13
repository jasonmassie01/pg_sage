//go:build e2e

// Layer B of the pipeline-coverage suite: every SQL shape the executor can
// be handed by a finding (Tier-1 rule, optimizer, or advisor) is driven
// through the REAL pipeline tail — sage.findings row → analyzer findings →
// Executor.RunCycle (trust gate, validation, execution, action_log) — and
// the side effect is verified against the live database.
//
// LLM generation is intentionally out of scope here (non-deterministic);
// each case injects the exact RecommendedSQL shape the LLM/rule produces,
// so the contract "a finding with this SQL comes all the way through
// actions correctly" is what is under test.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// executedOutcomes are the outcomes that mean "the SQL ran": actions with
// rollback SQL go straight into the monitoring window, others log success.
func isExecutedOutcome(outcome string) bool {
	return outcome == "success" || outcome == "monitoring"
}

func TestPipelineCoverage_SQLShapes(t *testing.T) {
	pool := pipelinePool(t)
	report := &checkReport{}
	defer report.dump(t, "Layer B — SQL shape pipeline checks")

	t.Run("B01_create_index_btree", func(t *testing.T) {
		shapeCreateIndexBtree(t, pool, report)
	})
	t.Run("B02_create_index_include_self_drop_guard", func(t *testing.T) {
		shapeCreateIndexIncludeSelfDrop(t, pool, report)
	})
	t.Run("B03_create_index_gin_jsonb", func(t *testing.T) {
		shapeCreateIndexGIN(t, pool, report)
	})
	t.Run("B04_create_index_hnsw_vector", func(t *testing.T) {
		shapeCreateIndexHNSW(t, pool, report)
	})
	t.Run("B05_create_index_partial", func(t *testing.T) {
		shapeCreateIndexPartial(t, pool, report)
	})
	t.Run("B06_drop_duplicate_index", func(t *testing.T) {
		shapeDropDuplicateIndex(t, pool, report)
	})
	t.Run("B07_vacuum_table_bloat", func(t *testing.T) {
		shapeVacuum(t, pool, report)
	})
	t.Run("B08_vacuum_freeze", func(t *testing.T) {
		shapeVacuumFreeze(t, pool, report)
	})
	t.Run("B09_analyze_stale_statistics", func(t *testing.T) {
		shapeAnalyze(t, pool, report)
	})
	t.Run("B10_alter_table_autovacuum", func(t *testing.T) {
		shapeAlterTableAutovacuum(t, pool, report)
	})
	t.Run("B11_alter_system_work_mem_reload", func(t *testing.T) {
		shapeAlterSystemWorkMem(t, pool, report)
	})
	t.Run("B12_terminate_backend", func(t *testing.T) {
		shapeTerminateBackend(t, pool, report)
	})
	t.Run("B13_cancel_backend", func(t *testing.T) {
		shapeCancelBackend(t, pool, report)
	})
	t.Run("B14_high_risk_never_executes", func(t *testing.T) {
		shapeHighRiskBlocked(t, pool, report)
	})
	t.Run("B15_observation_trust_blocks_safe", func(t *testing.T) {
		shapeObservationBlocked(t, pool, report)
	})
	t.Run("B16_tier3_moderate_off_blocks", func(t *testing.T) {
		shapeModerateFlagBlocked(t, pool, report)
	})
	t.Run("B17_empty_window_blocks_moderate", func(t *testing.T) {
		shapeEmptyWindowBlocked(t, pool, report)
	})
	t.Run("B18_multi_statement_rejected", func(t *testing.T) {
		shapeMultiStatementRejected(t, pool, report)
	})
	t.Run("B19_alter_role_rejected", func(t *testing.T) {
		shapeAlterRoleRejected(t, pool, report)
	})
	t.Run("B20_no_sql_finding_no_action", func(t *testing.T) {
		shapeNoSQLNoAction(t, pool, report)
	})
}

// --- positive shapes -------------------------------------------------------

func shapeCreateIndexBtree(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b01;
		CREATE TABLE shp_b01 (id bigint, v text);
		INSERT INTO shp_b01 SELECT g, g::text FROM generate_series(1,1000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b01",
		Title:          "missing index on shp_b01(id)",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b01_id_idx ON public.shp_b01 (id)",
		RollbackSQL:    "DROP INDEX CONCURRENTLY IF EXISTS shp_b01_id_idx",
		ActionRisk:     "moderate",
	})
	act, ok := latestActionFor(t, pool, "missing_index", "public.shp_b01")
	r.add(t, "CHECK-B01a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("btree CREATE INDEX executed (outcome=%s)", act.Outcome))
	am, valid, exists := indexAccessMethod(t, pool, "shp_b01_id_idx")
	r.add(t, "CHECK-B01b", exists && valid && am == "btree",
		"index shp_b01_id_idx exists, valid, am=btree")
}

func shapeCreateIndexIncludeSelfDrop(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b02;
		CREATE TABLE shp_b02 (cid bigint, total bigint);
		INSERT INTO shp_b02 SELECT g, g FROM generate_series(1,1000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	// Mirrors a real covering_index rec: drop_ddl is the NEW index's own
	// rollback. The executor must NOT consume it as an INCLUDE-upgrade
	// supersede (the self-drop bug fixed 2026-06-12).
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "covering_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b02",
		Title: "covering index on shp_b02",
		Detail: map[string]any{
			"drop_ddl": "DROP INDEX CONCURRENTLY IF EXISTS shp_b02_cov_idx",
		},
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b02_cov_idx " +
			"ON public.shp_b02 (cid) INCLUDE (total)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS shp_b02_cov_idx",
		ActionRisk:  "moderate",
	})
	act, ok := latestActionFor(t, pool, "covering_index", "public.shp_b02")
	r.add(t, "CHECK-B02a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("covering INCLUDE index executed (outcome=%s)", act.Outcome))
	_, valid, exists := indexAccessMethod(t, pool, "shp_b02_cov_idx")
	r.add(t, "CHECK-B02b", exists && valid,
		"index survives its own drop_ddl (self-drop guard)")
}

func shapeCreateIndexGIN(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b03;
		CREATE TABLE shp_b03 (payload jsonb);
		INSERT INTO shp_b03
		SELECT jsonb_build_object('k', g) FROM generate_series(1,1000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b03",
		Title: "gin index for jsonb containment",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b03_gin_idx " +
			"ON public.shp_b03 USING gin (payload jsonb_path_ops)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS shp_b03_gin_idx",
		ActionRisk:  "moderate",
	})
	act, ok := latestActionFor(t, pool, "missing_index", "public.shp_b03")
	r.add(t, "CHECK-B03a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("GIN jsonb_path_ops index executed (outcome=%s)", act.Outcome))
	am, valid, exists := indexAccessMethod(t, pool, "shp_b03_gin_idx")
	r.add(t, "CHECK-B03b", exists && valid && am == "gin",
		"index shp_b03_gin_idx exists, valid, am=gin")
}

func shapeCreateIndexHNSW(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		"CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Skipf("SKIPPED: pgvector unavailable on pipeline PG: %v "+
			"(runner should use a pgvector image)", err)
	}
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b04;
		CREATE TABLE shp_b04 (emb vector(8));
		INSERT INTO shp_b04
		SELECT ('['||g%10||',0,0,0,0,0,0,'||g%7||']')::vector
		  FROM generate_series(1,500) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b04",
		Title: "hnsw index for vector knn",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b04_hnsw_idx " +
			"ON public.shp_b04 USING hnsw (emb vector_l2_ops)",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS shp_b04_hnsw_idx",
		ActionRisk:  "moderate",
	})
	act, ok := latestActionFor(t, pool, "missing_index", "public.shp_b04")
	r.add(t, "CHECK-B04a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("HNSW vector index executed (outcome=%s)", act.Outcome))
	am, valid, exists := indexAccessMethod(t, pool, "shp_b04_hnsw_idx")
	r.add(t, "CHECK-B04b", exists && valid && am == "hnsw",
		"index shp_b04_hnsw_idx exists, valid, am=hnsw")
}

func shapeCreateIndexPartial(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b05;
		CREATE TABLE shp_b05 (id bigint, deleted_at timestamptz);
		INSERT INTO shp_b05 SELECT g, NULL FROM generate_series(1,1000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "partial_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b05",
		Title: "partial index on live rows",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b05_live_idx " +
			"ON public.shp_b05 (id) WHERE deleted_at IS NULL",
		RollbackSQL: "DROP INDEX CONCURRENTLY IF EXISTS shp_b05_live_idx",
		ActionRisk:  "moderate",
	})
	act, ok := latestActionFor(t, pool, "partial_index", "public.shp_b05")
	r.add(t, "CHECK-B05a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("partial index executed (outcome=%s)", act.Outcome))
	_, valid, exists := indexAccessMethod(t, pool, "shp_b05_live_idx")
	r.add(t, "CHECK-B05b", exists && valid, "partial index exists and valid")
}

func shapeDropDuplicateIndex(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b06;
		CREATE TABLE shp_b06 (s text);
		CREATE INDEX shp_b06_dup_a ON shp_b06 (s);
		CREATE INDEX shp_b06_dup_b ON shp_b06 (s);`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "duplicate_index", Severity: "warning",
		ObjectType: "index", ObjectIdentifier: "public.shp_b06_dup_a",
		Title:          "duplicate of shp_b06_dup_b",
		RecommendedSQL: "DROP INDEX CONCURRENTLY public.shp_b06_dup_a;",
		RollbackSQL:    "CREATE INDEX CONCURRENTLY shp_b06_dup_a ON public.shp_b06 (s)",
		ActionRisk:     "safe",
	})
	act, ok := latestActionFor(t, pool, "duplicate_index", "public.shp_b06_dup_a")
	r.add(t, "CHECK-B06a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("duplicate DROP INDEX executed (outcome=%s)", act.Outcome))
	_, _, aExists := indexAccessMethod(t, pool, "shp_b06_dup_a")
	_, bValid, bExists := indexAccessMethod(t, pool, "shp_b06_dup_b")
	r.add(t, "CHECK-B06b", !aExists && bExists && bValid,
		"dup_a dropped, dup_b intact")
}

func shapeVacuum(t *testing.T, pool *pgxpool.Pool, r *checkReport) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b07;
		CREATE TABLE shp_b07 (id int) WITH (autovacuum_enabled = false);
		INSERT INTO shp_b07 SELECT g FROM generate_series(1,5000) g;
		DELETE FROM shp_b07 WHERE id % 2 = 0;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "table_bloat", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b07",
		Title:          "dead tuples on shp_b07",
		RecommendedSQL: `VACUUM "public"."shp_b07";`,
		ActionRisk:     "safe",
	})
	act, ok := latestActionFor(t, pool, "table_bloat", "public.shp_b07")
	r.add(t, "CHECK-B07a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("VACUUM executed (outcome=%s)", act.Outcome))
	lastVacuum := scalarString(t, pool,
		`SELECT coalesce(last_vacuum::text,'') FROM pg_stat_user_tables
		  WHERE relname = 'shp_b07'`)
	r.add(t, "CHECK-B07b", lastVacuum != "",
		"pg_stat_user_tables.last_vacuum set (vacuum really ran)")
}

func shapeVacuumFreeze(t *testing.T, pool *pgxpool.Pool, r *checkReport) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b08;
		CREATE TABLE shp_b08 (id int) WITH (autovacuum_enabled = false);
		INSERT INTO shp_b08 SELECT g FROM generate_series(1,2000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "wraparound_freeze", Severity: "critical",
		ObjectType: "table", ObjectIdentifier: "public.shp_b08",
		Title:          "xid age on shp_b08",
		RecommendedSQL: `VACUUM (FREEZE) "public"."shp_b08";`,
		ActionRisk:     "safe",
	})
	act, ok := latestActionFor(t, pool, "wraparound_freeze", "public.shp_b08")
	r.add(t, "CHECK-B08a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("VACUUM (FREEZE) executed (outcome=%s)", act.Outcome))
	frozen := scalarString(t, pool,
		`SELECT (age(relfrozenxid) < 100)::text FROM pg_class
		  WHERE relname = 'shp_b08'`)
	r.add(t, "CHECK-B08b", frozen == "true",
		"relfrozenxid advanced (freeze really ran)")
}

func shapeAnalyze(t *testing.T, pool *pgxpool.Pool, r *checkReport) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b09;
		CREATE TABLE shp_b09 (id int) WITH (autovacuum_enabled = false);
		INSERT INTO shp_b09 SELECT g FROM generate_series(1,5000) g;`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "stale_statistics", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b09",
		Title:          "never analyzed",
		RecommendedSQL: `ANALYZE "public"."shp_b09";`,
		ActionRisk:     "safe",
	})
	act, ok := latestActionFor(t, pool, "stale_statistics", "public.shp_b09")
	r.add(t, "CHECK-B09a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("ANALYZE executed (outcome=%s)", act.Outcome))
	lastAnalyze := scalarString(t, pool,
		`SELECT coalesce(last_analyze::text,'') FROM pg_stat_user_tables
		  WHERE relname = 'shp_b09'`)
	r.add(t, "CHECK-B09b", lastAnalyze != "",
		"pg_stat_user_tables.last_analyze set (analyze really ran)")
}

func shapeAlterTableAutovacuum(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b10;
		CREATE TABLE shp_b10 (id int);`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "autovacuum_tuning", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b10",
		Title: "tune scale factor",
		RecommendedSQL: `ALTER TABLE "public"."shp_b10" ` +
			`SET (autovacuum_vacuum_scale_factor = 0.05);`,
		RollbackSQL: `ALTER TABLE "public"."shp_b10" ` +
			`RESET (autovacuum_vacuum_scale_factor);`,
		ActionRisk: "safe",
	})
	act, ok := latestActionFor(t, pool, "autovacuum_tuning", "public.shp_b10")
	r.add(t, "CHECK-B10a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("ALTER TABLE SET executed (outcome=%s)", act.Outcome))
	opts := scalarString(t, pool,
		`SELECT coalesce(array_to_string(reloptions, ','), '')
		   FROM pg_class WHERE relname = 'shp_b10'`)
	r.add(t, "CHECK-B10b",
		strings.Contains(opts, "autovacuum_vacuum_scale_factor=0.05"),
		"reloptions carry the new scale factor")
}

func shapeAlterSystemWorkMem(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	before := scalarString(t, pool, "SHOW work_mem")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "ALTER SYSTEM RESET work_mem")
		_, _ = pool.Exec(ctx, "SELECT pg_reload_conf()")
	})
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "memory_tuning", Severity: "warning",
		ObjectType: "configuration", ObjectIdentifier: "instance:work_mem",
		Title:          "raise work_mem for temp spills",
		RecommendedSQL: "ALTER SYSTEM SET work_mem = '48MB';",
		RollbackSQL:    "ALTER SYSTEM RESET work_mem;",
		ActionRisk:     "moderate",
	})
	act, ok := latestActionFor(t, pool, "memory_tuning", "instance:work_mem")
	r.add(t, "CHECK-B11a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("ALTER SYSTEM executed (outcome=%s reason=%s)",
			act.Outcome, act.RollbackReason))
	// The config-apply path must have reloaded: a NEW session sees the
	// value without any restart (work_mem is sighup/user-settable).
	deadline := time.Now().Add(10 * time.Second)
	applied := false
	for time.Now().Before(deadline) {
		if scalarString(t, pool, "SHOW work_mem") == "48MB" {
			applied = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.add(t, "CHECK-B11b", applied,
		fmt.Sprintf("work_mem in effect via pg_reload_conf (before=%s)", before))
}

func shapeTerminateBackend(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	victim, err := pgx.Connect(ctx, pipelineURL(t))
	if err != nil {
		t.Fatalf("victim connect: %v", err)
	}
	defer victim.Close(context.Background())
	var pid int
	if err := victim.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("victim pid: %v", err)
	}
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "connection_leak", Severity: "warning",
		ObjectType: "backend", ObjectIdentifier: fmt.Sprintf("pid:%d", pid),
		Title:          "idle-in-transaction backend",
		RecommendedSQL: fmt.Sprintf("SELECT pg_terminate_backend(%d);", pid),
		ActionRisk:     "safe",
	})
	act, ok := latestActionFor(t, pool, "connection_leak",
		fmt.Sprintf("pid:%d", pid))
	r.add(t, "CHECK-B12a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("pg_terminate_backend executed (outcome=%s)", act.Outcome))
	gone := scalarString(t, pool, fmt.Sprintf(
		`SELECT (count(*) = 0)::text FROM pg_stat_activity WHERE pid = %d`, pid))
	r.add(t, "CHECK-B12b", gone == "true", "victim backend terminated")
}

func shapeCancelBackend(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	victim, err := pgx.Connect(ctx, pipelineURL(t))
	if err != nil {
		t.Fatalf("victim connect: %v", err)
	}
	defer victim.Close(context.Background())
	var pid int
	if err := victim.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("victim pid: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, qErr := victim.Exec(context.Background(), "SELECT pg_sleep(30)")
		errCh <- qErr
	}()
	// Wait until the sleep is visibly active before cancelling it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state := scalarString(t, pool, fmt.Sprintf(
			`SELECT coalesce(max(state),'') FROM pg_stat_activity
			  WHERE pid = %d AND query LIKE '%%pg_sleep%%'`, pid))
		if state == "active" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "runaway_query", Severity: "warning",
		ObjectType: "backend", ObjectIdentifier: fmt.Sprintf("pid:%d", pid),
		Title:          "runaway query",
		RecommendedSQL: fmt.Sprintf("SELECT pg_cancel_backend(%d);", pid),
		ActionRisk:     "moderate",
	})
	act, ok := latestActionFor(t, pool, "runaway_query",
		fmt.Sprintf("pid:%d", pid))
	r.add(t, "CHECK-B13a", ok && isExecutedOutcome(act.Outcome),
		fmt.Sprintf("pg_cancel_backend executed (outcome=%s)", act.Outcome))
	select {
	case qErr := <-errCh:
		r.add(t, "CHECK-B13b",
			qErr != nil && strings.Contains(qErr.Error(), "cancel"),
			fmt.Sprintf("victim query cancelled (err=%v)", qErr))
	case <-time.After(15 * time.Second):
		r.add(t, "CHECK-B13b", false,
			"victim query cancelled (timed out still sleeping)")
	}
}

// --- negative shapes (the gate must HOLD) ----------------------------------

func shapeHighRiskBlocked(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b14;
		CREATE TABLE shp_b14 (a int, b int);
		CREATE INDEX shp_b14_subset ON shp_b14 (a);
		CREATE INDEX shp_b14_wide ON shp_b14 (a, b);`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "duplicate_index", Severity: "info",
		ObjectType: "index", ObjectIdentifier: "public.shp_b14_subset",
		Title:          "subset of shp_b14_wide (advisory)",
		RecommendedSQL: "DROP INDEX CONCURRENTLY public.shp_b14_subset;",
		ActionRisk:     "high_risk",
	})
	_, acted := latestActionFor(t, pool, "duplicate_index", "public.shp_b14_subset")
	_, _, exists := indexAccessMethod(t, pool, "shp_b14_subset")
	r.add(t, "CHECK-B14", !acted && exists,
		"high_risk finding produced NO action; index untouched")
}

func shapeObservationBlocked(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b15;
		CREATE TABLE shp_b15 (id int) WITH (autovacuum_enabled = false);
		INSERT INTO shp_b15 SELECT g FROM generate_series(1,1000) g;
		DELETE FROM shp_b15 WHERE id % 2 = 0;`)
	cfg := autonomousConfig()
	cfg.Trust.Level = "observation"
	an, ex := newPipelineExecutor(t, pool, cfg)
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "table_bloat", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b15",
		Title:          "bloat under observation trust",
		RecommendedSQL: `VACUUM "public"."shp_b15";`,
		ActionRisk:     "safe",
	})
	_, acted := latestActionFor(t, pool, "table_bloat", "public.shp_b15")
	r.add(t, "CHECK-B15", !acted,
		"observation trust blocks even SAFE actions")
}

func shapeModerateFlagBlocked(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b16;
		CREATE TABLE shp_b16 (id int);`)
	cfg := autonomousConfig()
	cfg.Trust.Tier3Moderate = false
	an, ex := newPipelineExecutor(t, pool, cfg)
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b16",
		Title:          "moderate with tier3_moderate=false",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b16_idx ON public.shp_b16 (id)",
		ActionRisk:     "moderate",
	})
	_, acted := latestActionFor(t, pool, "missing_index", "public.shp_b16")
	_, _, exists := indexAccessMethod(t, pool, "shp_b16_idx")
	r.add(t, "CHECK-B16", !acted && !exists,
		"tier3_moderate=false blocks moderate actions")
}

func shapeEmptyWindowBlocked(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b17;
		CREATE TABLE shp_b17 (id int);`)
	cfg := autonomousConfig()
	cfg.Trust.MaintenanceWindow = ""
	an, ex := newPipelineExecutor(t, pool, cfg)
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_b17",
		Title:          "moderate with empty maintenance window",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_b17_idx ON public.shp_b17 (id)",
		ActionRisk:     "moderate",
	})
	_, acted := latestActionFor(t, pool, "missing_index", "public.shp_b17")
	_, _, exists := indexAccessMethod(t, pool, "shp_b17_idx")
	r.add(t, "CHECK-B17", !acted && !exists,
		"empty maintenance window blocks moderate actions")
}

func shapeMultiStatementRejected(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_b18;
		CREATE TABLE shp_b18 (id int);`)
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "memory_tuning", Severity: "warning",
		ObjectType: "configuration", ObjectIdentifier: "instance:multi",
		Title: "multi-statement must be rejected",
		RecommendedSQL: "ALTER SYSTEM SET work_mem = '32MB'; " +
			"SELECT pg_reload_conf();",
		ActionRisk: "moderate",
	})
	act, acted := latestActionFor(t, pool, "memory_tuning", "instance:multi")
	r.add(t, "CHECK-B18",
		acted && act.Outcome == "failed" &&
			strings.Contains(act.RollbackReason, "multi-statement"),
		fmt.Sprintf("multi-statement SQL rejected by validation "+
			"(outcome=%s reason=%s)", act.Outcome, act.RollbackReason))
}

func shapeAlterRoleRejected(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	mustExec(t, pool, `DROP ROLE IF EXISTS shp_b19_role;
		CREATE ROLE shp_b19_role;`)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS shp_b19_role")
	})
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	// work_mem_promotion emits ALTER ROLE ... SET work_mem, which is NOT in
	// the executor allowlist. The pipeline must fail it cleanly (logged
	// failed action) rather than execute or crash.
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "work_mem_promotion", Severity: "warning",
		ObjectType: "role", ObjectIdentifier: "role:shp_b19_role",
		Title:          "promote work_mem for role",
		RecommendedSQL: "ALTER ROLE shp_b19_role SET work_mem = '64MB';",
		ActionRisk:     "moderate",
	})
	act, acted := latestActionFor(t, pool, "work_mem_promotion", "role:shp_b19_role")
	roleCfg := scalarString(t, pool,
		`SELECT coalesce(array_to_string(rolconfig, ','), '')
		   FROM pg_roles WHERE rolname = 'shp_b19_role'`)
	r.add(t, "CHECK-B19",
		acted && act.Outcome == "failed" && roleCfg == "",
		fmt.Sprintf("ALTER ROLE rejected by allowlist, role untouched "+
			"(outcome=%s reason=%s)", act.Outcome, act.RollbackReason))
}

func shapeNoSQLNoAction(
	t *testing.T, pool *pgxpool.Pool, r *checkReport,
) {
	an, ex := newPipelineExecutor(t, pool, autonomousConfig())
	driveFinding(t, pool, an, ex, analyzer.Finding{
		Category: "slow_query", Severity: "warning",
		ObjectType: "query", ObjectIdentifier: "queryid:b20",
		Title:      "advisory slow query (no SQL)",
		ActionRisk: "safe",
	})
	_, acted := latestActionFor(t, pool, "slow_query", "queryid:b20")
	r.add(t, "CHECK-B20", !acted,
		"finding without RecommendedSQL produces no action")
}

// silence unused-import guards when cases are skipped on minimal images
var _ = config.DefaultConfig
