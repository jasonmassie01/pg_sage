package tuner

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

var (
	tunerTestPool     *pgxpool.Pool
	tunerTestPoolOnce sync.Once
	tunerTestPoolErr  error
)

const tunerCoverageQueryID int64 = 999999999

func tunerTestDSN() string {
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
}

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
	tunerTestPoolOnce.Do(func() {
		dsn := tunerTestDSN()
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
		schema.ReleaseAdvisoryLock(ctx, tunerTestPool)
	})
	if tunerTestPoolErr != nil {
		t.Skipf("database unavailable: %v", tunerTestPoolErr)
	}
	return tunerTestPool, ctx
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

	deleteSQL := BuildDeleteSQL(queryID)
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
// processCandidate finds a high_plan_time issue.
func TestFunctional_Coverage_DB_ProcessCandidate_WithSymptom(
	t *testing.T,
) {
	pool, ctx := requireTunerDB(t)
	cleanTunerCoverageHints(t, pool, ctx)
	t.Cleanup(func() {
		cleanTunerCoverageHints(t, pool, context.Background())
	})

	hp := &HintPlanAvailability{Available: false}
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
	if f.Category != "query_tuning" {
		t.Errorf("expected category=query_tuning, got %s", f.Category)
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

// TestFunctional_Coverage_DB_FetchSystemContext tests fetching
// system GUCs from a real PG.
func TestFunctional_Coverage_DB_FetchSystemContext(t *testing.T) {
	pool, ctx := requireTunerDB(t)

	sys := fetchSystemContext(ctx, pool)
	if sys.MaxConnections <= 0 {
		t.Errorf("expected positive max_connections, got %d",
			sys.MaxConnections)
	}
	if sys.WorkMem == "" {
		t.Error("expected non-empty work_mem")
	}
	if sys.SharedBuffers == "" {
		t.Error("expected non-empty shared_buffers")
	}
}
