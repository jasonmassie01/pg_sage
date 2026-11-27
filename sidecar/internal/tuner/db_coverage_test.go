package tuner

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

var (
	tunerTestPool     *pgxpool.Pool
	tunerTestPoolOnce sync.Once
	tunerTestPoolErr  error
)

const tunerCoverageQueryID int64 = 999999999

func cleanTunerCoverageHints(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.query_hints WHERE queryid = $1",
		tunerCoverageQueryID)
	if err != nil {
		t.Fatalf("clean tuner coverage hints: %v", err)
	}
}

func requireTunerDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.SkipUnlessLive(t)
	tunerTestPoolOnce.Do(func() {
		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			tunerTestPoolErr = fmt.Errorf("parsing DSN: %w", err)
			return
		}
		poolCfg.MaxConns = 2
		tunerTestPool, tunerTestPoolErr = pgxpool.NewWithConfig(
			ctx, poolCfg,
		)
		if tunerTestPoolErr != nil {
			return
		}
		if err := tunerTestPool.Ping(ctx); err != nil {
			tunerTestPoolErr = fmt.Errorf("ping: %w", err)
			tunerTestPool.Close()
			tunerTestPool = nil
			return
		}
		if err := schema.Bootstrap(ctx, tunerTestPool); err != nil {
			tunerTestPoolErr = fmt.Errorf("bootstrap: %w", err)
			tunerTestPool.Close()
			tunerTestPool = nil
			return
		}
	})
	if tunerTestPoolErr != nil {
		t.Skipf("database unavailable: %v", tunerTestPoolErr)
	}
	return tunerTestPool, ctx
}

// exclusiveTunerDB is requireTunerDB on a fresh database of the exclusive
// test server, held for t alone (testdb.Exclusive): for tests that reset
// every database's pg_stat_statements (ci-13).
func exclusiveTunerDB(t *testing.T, reason string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	poolCfg, err := pgxpool.ParseConfig(testdb.Exclusive(t, reason))
	if err != nil {
		t.Fatalf("parse the exclusive test DSN: %v", err)
	}
	poolCfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap the exclusive test database: %v", err)
	}
	return pool, ctx
}

// TestFunctional_Coverage_DB_FetchPlanJSON tests fetchPlanJSON with a
// real PostgreSQL connection.
func TestFunctional_Coverage_DB_FetchPlanJSON(t *testing.T) {
	pool, ctx := requireTunerDB(t)

	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// No matching row → returns empty string.
	result := tuner.fetchPlanJSON(ctx, tunerCoverageQueryID)
	if result != "" {
		t.Errorf("expected empty string for unknown queryid, got %q",
			result)
	}
}

func TestFunctional_Coverage_DB_HintPlanSQLUsesQueryIDSchema(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	requireQueryIDHintTable(t, pool)
	queryID := int64(8809001)

	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS hint_plan")
	if err != nil {
		t.Fatalf("create hint_plan schema: %v", err)
	}
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS hint_plan.hints (
		id bigserial PRIMARY KEY,
		query_id bigint,
		application_name text,
		hints text
	)`)
	if err != nil {
		t.Fatalf("create hint_plan.hints table: %v", err)
	}
	_, _ = pool.Exec(ctx,
		"DELETE FROM hint_plan.hints WHERE query_id = $1", queryID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM hint_plan.hints WHERE query_id = $1", queryID)
	})

	insertSQL := BuildInsertSQL(queryID, "IndexScan(t idx_t_id)")
	if _, err := pool.Exec(ctx, insertSQL); err != nil {
		t.Fatalf("execute generated insert SQL: %v\nSQL: %s", err, insertSQL)
	}

	var hints string
	err = pool.QueryRow(ctx,
		`SELECT hints FROM hint_plan.hints
		  WHERE query_id = $1 AND application_name = ''`,
		queryID).Scan(&hints)
	if err != nil {
		t.Fatalf("query inserted hint: %v", err)
	}
	if hints != "IndexScan(t idx_t_id)" {
		t.Fatalf("hint mismatch: got %q", hints)
	}

	deleteSQL := BuildDeleteSQL(queryID, "IndexScan(t idx_t_id)")
	if _, err := pool.Exec(ctx, deleteSQL); err != nil {
		t.Fatalf("execute generated delete SQL: %v\nSQL: %s", err, deleteSQL)
	}

	var count int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM hint_plan.hints WHERE query_id = $1",
		queryID).Scan(&count)
	if err != nil {
		t.Fatalf("count deleted hint: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected generated delete to remove hint, count=%d", count)
	}
}

// TestFunctional_Coverage_DB_ScanPlanForQuery tests scanPlanForQuery
// with no explain cache rows.
func TestFunctional_Coverage_DB_ScanPlanForQuery(t *testing.T) {
	pool, ctx := requireTunerDB(t)

	tuner := New(pool, TunerConfig{}, nil, noopLogFn)

	// No cached plan → returns nil symptoms.
	symptoms := tuner.scanPlanForQuery(ctx, tunerCoverageQueryID)
	if len(symptoms) != 0 {
		t.Errorf("expected 0 symptoms for unknown queryid, got %d",
			len(symptoms))
	}
}

// TestFunctional_Coverage_DB_GatherSymptoms tests gatherSymptoms
// with no matching data.
func TestFunctional_Coverage_DB_GatherSymptoms(t *testing.T) {
	pool, ctx := requireTunerDB(t)

	tuner := New(pool, TunerConfig{
		PlanTimeRatio: 2.0,
		MinQueryCalls: 5,
	}, nil, noopLogFn)

	c := candidate{
		QueryID:      tunerCoverageQueryID,
		Query:        "SELECT 1",
		Calls:        1,
		MeanExecTime: 100.0,
		MeanPlanTime: 0,
	}
	symptoms := tuner.gatherSymptoms(ctx, c)
	if len(symptoms) != 0 {
		t.Errorf("expected 0 symptoms, got %d", len(symptoms))
	}
}

// TestFunctional_Coverage_DB_GatherSymptoms_HighPlanTime tests
// gatherSymptoms detecting high plan time without plan cache.
func TestFunctional_Coverage_DB_GatherSymptoms_HighPlanTime(
	t *testing.T,
) {
	pool, ctx := requireTunerDB(t)

	tuner := New(pool, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 1,
	}, nil, noopLogFn)

	c := candidate{
		QueryID:      tunerCoverageQueryID,
		Query:        "SELECT 1",
		Calls:        10,
		MeanExecTime: 10.0,
		MeanPlanTime: 100.0, // >> 10 * 0.5
	}
	symptoms := tuner.gatherSymptoms(ctx, c)
	found := false
	for _, s := range symptoms {
		if s.Kind == SymptomHighPlanTime {
			found = true
		}
	}
	if !found {
		t.Error("expected high_plan_time symptom")
	}
}

// TestFunctional_Coverage_DB_ProcessCandidate_NoSymptoms tests
// processCandidate returns nil when no symptoms found.
func TestFunctional_Coverage_DB_ProcessCandidate_NoSymptoms(
	t *testing.T,
) {
	pool, ctx := requireTunerDB(t)

	tuner := New(pool, TunerConfig{
		PlanTimeRatio: 2.0,
		MinQueryCalls: 100,
	}, &HintPlanAvailability{}, noopLogFn)

	c := candidate{
		QueryID:      tunerCoverageQueryID,
		Query:        "SELECT 1",
		Calls:        1,
		MeanExecTime: 100.0,
		MeanPlanTime: 0,
	}
	findings := tuner.processCandidate(ctx, c)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for no-symptom candidate, got %d",
			len(findings))
	}
}

// TestFunctional_Coverage_DB_ProcessCandidate_WithSymptom tests
// processCandidate finds a high_plan_time issue. Since tuning-11 it is
// parameterization advice, not a query_tuning hint (this test expected
// query_tuning before).
func TestFunctional_Coverage_DB_ProcessCandidate_WithSymptom(
	t *testing.T,
) {
	pool, ctx := requireTunerDB(t)
	cleanTunerCoverageHints(t, pool, ctx)
	t.Cleanup(func() {
		cleanTunerCoverageHints(t, pool, context.Background())
	})

	hp := &HintPlanAvailability{ApplicationEffect: false}
	tuner := New(pool, TunerConfig{
		PlanTimeRatio: 0.5,
		MinQueryCalls: 1,
	}, hp, noopLogFn)

	c := candidate{
		QueryID:      tunerCoverageQueryID,
		Query:        "SELECT 1",
		Calls:        10,
		MeanExecTime: 10.0,
		MeanPlanTime: 100.0,
	}
	findings := tuner.processCandidate(ctx, c)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.Category != "query_parameterization" || f.Detail["reason"] != "plan_time_share" {
		t.Errorf("expected query_parameterization/plan_time_share, got %s/%v",
			f.Category, f.Detail["reason"])
	}
}

// TestFunctional_Coverage_DB_DetectHintPlan tests detection with
// a real PG connection (pg_hint_plan likely absent).
func TestFunctional_Coverage_DB_DetectHintPlan(t *testing.T) {
	pool, ctx := requireTunerDB(t)

	result, err := DetectHintPlan(ctx, pool)
	if err != nil {
		t.Fatalf("DetectHintPlan error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Without pg_hint_plan installed, expect unavailable.
	if result.Method != "unavailable" && result.Method != "shared_preload" &&
		result.Method != "session_load" {
		t.Errorf("unexpected method: %s", result.Method)
	}
}

// tuning-09 integration: a real 50-row nested loop whose estimate is off by
// far more than 10x (correlated filter columns) gives no hint at the default
// threshold; at threshold 10 the same captured plan earns HashJoin(o i).
func TestFunctional_Coverage_DB_SmallNestedLoopGivesNoHint(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	cleanTunerCoverageHints(t, pool, ctx)
	planJSON := smallNestedLoopPlan(t, pool, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.explain_cache (queryid, plan_json, source)
		VALUES ($1, $2, 'test')`, tunerCoverageQueryID, planJSON); err != nil {
		t.Fatalf("cache plan: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.explain_cache WHERE queryid = $1",
			tunerCoverageQueryID)
		cleanTunerCoverageHints(t, pool, bg)
	})
	c := candidate{QueryID: tunerCoverageQueryID, Query: "SELECT o.* FROM t_outer o",
		Calls: 500, MeanExecTime: 5}
	hp := &HintPlanAvailability{}
	if got := New(pool, TunerConfig{}, hp, noopLogFn).processCandidate(ctx, c); len(got) != 0 {
		t.Fatalf("50-row nested loop at the default threshold: %d findings %+v", len(got), got)
	}
	var proposals int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.query_hints WHERE queryid = $1",
		tunerCoverageQueryID).Scan(&proposals); err != nil || proposals != 0 {
		t.Fatalf("query_hints rows = %d (err %v), want 0", proposals, err)
	}
	low := New(pool, TunerConfig{NestedLoopRowThreshold: 10}, hp, noopLogFn)
	got := low.processCandidate(ctx, c)
	if len(got) != 1 || got[0].Detail["hint_directive"] != "HashJoin(o i)" {
		t.Fatalf("threshold 10 (positive control): findings %+v, want one HashJoin(o i)", got)
	}
}

// smallNestedLoopPlan runs EXPLAIN (ANALYZE, FORMAT JSON) on a forced
// nested loop: x and y are equal, so x = 1 AND y = 1 is estimated at about
// 1 row of 2,000 but returns 50; each returns one inner row by primary key.
func smallNestedLoopPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context) []byte {
	t.Helper()
	setup := []string{
		"DROP SCHEMA IF EXISTS tuning09_nl CASCADE",
		"CREATE SCHEMA tuning09_nl",
		`CREATE TABLE tuning09_nl.t_outer AS SELECT g AS id, g % 40 AS x, g % 40 AS y
		 FROM generate_series(1, 2000) g`,
		`CREATE TABLE tuning09_nl.t_inner AS SELECT g AS id, md5(g::text) AS v
		 FROM generate_series(1, 2000) g`,
		"ALTER TABLE tuning09_nl.t_inner ADD PRIMARY KEY (id)",
		"ANALYZE tuning09_nl.t_outer", "ANALYZE tuning09_nl.t_inner",
	}
	for _, s := range setup {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS tuning09_nl CASCADE")
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []string{"SET LOCAL enable_hashjoin = off",
		"SET LOCAL enable_mergejoin = off", "SET LOCAL statement_timeout = '30s'"} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var plan []byte
	err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON)
		SELECT o.id, i.v FROM tuning09_nl.t_outer o
		JOIN tuning09_nl.t_inner i ON i.id = o.id WHERE o.x = 1 AND o.y = 1`).Scan(&plan)
	if err != nil {
		t.Fatalf("explain analyze: %v", err)
	}
	requireMisestimatedNestedLoop(t, plan)
	return plan
}

// requireMisestimatedNestedLoop proves the fixture is one the old rule
// flagged: a Nested Loop whose actual rows exceed 10x its estimate.
func requireMisestimatedNestedLoop(t *testing.T, plan []byte) {
	t.Helper()
	root, err := parsePlanRoot(plan)
	if err != nil {
		t.Fatalf("parse plan: %v", err)
	}
	if root.NodeType != "Nested Loop" || root.ActualRows == nil ||
		*root.ActualRows != 50 || *root.ActualRows <= float64(root.PlanRows*10) {
		t.Fatalf("fixture is not a 50-row nested loop off by >10x: %s", plan)
	}
}
