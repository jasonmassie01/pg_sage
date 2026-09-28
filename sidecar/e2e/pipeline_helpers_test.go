//go:build e2e

// Pipeline-coverage helpers shared by pipeline_shapes_test.go (in-process
// executor SQL-shape coverage) and pipeline_binary_test.go (full-binary
// detection→action coverage).
//
// Both layers run against a DEDICATED throwaway Postgres given by
// PIPELINE_PG_URL (see scripts/run_pipeline_coverage.sh). They are
// destructive (ALTER SYSTEM, terminate backends) and must never point at a
// database anyone cares about.
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/schema"
)

// pipelineURL returns the throwaway-Postgres URL or skips the test with an
// explicit reason (no silent skips: the runner script always sets it).
func pipelineURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("SAGE_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("SKIPPED: SAGE_TEST_DATABASE_URL not set — run via " +
			"scripts/run_pipeline_coverage.sh (dedicated container)")
	}
	return u
}

// pipelinePool connects, bootstraps the sage schema, and registers cleanup.
// Bootstrap takes a session-scoped advisory lock that is only released when
// the pool closes — never use this in the same test as the real binary
// (its own bootstrap would dead-wait on our lock); use rawPipelinePool there.
func pipelinePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := rawPipelinePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A previous hard-killed binary (Layer A teardown) can leave orphaned
	// idle-in-transaction backends that Postgres hasn't reaped yet; a
	// CONCURRENTLY migration would wait on their snapshots. Clear exactly
	// those (plain idle sessions — like this pool's own — are harmless).
	_, _ = pool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		  WHERE datname = current_database()
		    AND pid <> pg_backend_pid()
		    AND state IN ('idle in transaction',
		                  'idle in transaction (aborted)')`)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("schema bootstrap: %v", err)
	}
	return pool
}

// rawPipelinePool connects WITHOUT bootstrapping the sage schema.
func rawPipelinePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, pipelineURL(t))
	if err != nil {
		t.Fatalf("connect %s: %v", pipelineURL(t), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping pipeline pg: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mustExec runs SQL and fails the test on error.
func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", truncSQL(sql), err)
	}
}

func truncSQL(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

// autonomousConfig returns a config under which SAFE and MODERATE actions
// auto-execute: autonomous trust, both tier3 flags, always-open window.
func autonomousConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe = true
	cfg.Trust.Tier3Moderate = true
	cfg.Trust.MaintenanceWindow = "always"
	// Keep the rollback window far away so MonitorAndRollback goroutines
	// never fire mid-test; Shutdown() aborts them at cleanup.
	cfg.Trust.RollbackWindowMinutes = 120
	return cfg
}

// rampStart30DaysPlus is comfortably past the 31-day MODERATE ramp.
var rampStart30DaysPlus = time.Now().AddDate(0, -3, 0)

// pipelineDatabases names each pipeline executor's database identity. The
// suite shares one PostgreSQL database, so every scenario gets its own
// recommendation identity: a later executor never acts on an earlier
// scenario's durable recommendations.
var pipelineDatabases sync.Map // *executor.Executor -> string

// newPipelineExecutor builds a real Analyzer+Executor pair around the pool.
// The analyzer's rule cycle is exercised by the full-binary layer; here a
// scenario proposes its finding as a durable recommendation directly.
func newPipelineExecutor(
	t *testing.T, pool *pgxpool.Pool, cfg *config.Config,
) (*analyzer.Analyzer, *executor.Executor) {
	t.Helper()
	logf := func(level, format string, args ...any) {
		t.Logf("[%s] "+format, append([]any{level}, args...)...)
	}
	an := analyzer.New(pool, cfg, nil, nil, nil, nil, nil, logf)
	ex := executor.New(pool, cfg, rampStart30DaysPlus, logf)
	database := fmt.Sprintf("e2e_%s_%d", t.Name(), time.Now().UnixNano())
	an.WithDatabaseName(database)
	ex.WithDatabaseName(database)
	pipelineDatabases.Store(ex, database)
	// As in production, the standing gate over the DB-stored policy is the
	// only authority; without it the executor fails closed (G4-I01).
	if err := ex.EnableStandingPolicy(context.Background(), cfg.Policy.Profile, nil); err != nil {
		t.Fatalf("enable standing policy: %v", err)
	}
	// Each scenario starts with an empty 24h usage window. The suite shares
	// one database, and its earlier actions would otherwise exhaust the
	// profile's blast radius (20 tables/24h) and park later scenarios.
	// Rows are kept as evidence, just moved outside the window.
	if _, err := pool.Exec(context.Background(), `UPDATE sage.action_log
		SET executed_at = executed_at - interval '25 hours'
		WHERE executed_at > now() - interval '24 hours'`); err != nil {
		t.Fatalf("age prior scenario actions: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = ex.Shutdown(ctx)
	})
	return an, ex
}

// driveFinding persists the finding and its durable recommendation, as
// the analyzer's cycle does, and runs one synchronous executor cycle,
// which acts on the durable recommendation (C07).
func driveFinding(
	t *testing.T,
	pool *pgxpool.Pool,
	_ *analyzer.Analyzer,
	ex *executor.Executor,
	f analyzer.Finding,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := analyzer.UpsertFindings(ctx, pool, []analyzer.Finding{f}); err != nil {
		t.Fatalf("upsert finding %s/%s: %v", f.Category, f.ObjectIdentifier, err)
	}
	database, _ := pipelineDatabases.Load(ex)
	name, _ := database.(string)
	if _, err := recommendation.NewStore(pool).Propose(ctx,
		analyzer.RecommendationProposal(name, f)); err != nil {
		t.Fatalf("propose recommendation %s/%s: %v", f.Category, f.ObjectIdentifier, err)
	}
	ex.RunCycle(ctx, false)
}

// actionRecord is what the pipeline wrote to sage.action_log for a finding.
type actionRecord struct {
	ID             int64
	ActionType     string
	Outcome        string
	SQLExecuted    string
	RollbackReason string
}

// latestActionFor returns the newest action logged for the finding matching
// (category, objectID), or ok=false when none was logged.
func latestActionFor(
	t *testing.T, pool *pgxpool.Pool, category, objectID string,
) (actionRecord, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var rec actionRecord
	err := pool.QueryRow(ctx,
		`SELECT a.id, a.action_type, a.outcome, a.sql_executed,
		        coalesce(a.rollback_reason, '')
		   FROM sage.action_log a
		   JOIN sage.findings f ON f.id::text = a.finding_id::text
		  WHERE f.category = $1 AND f.object_identifier = $2
		  ORDER BY a.executed_at DESC
		  LIMIT 1`,
		category, objectID,
	).Scan(&rec.ID, &rec.ActionType, &rec.Outcome,
		&rec.SQLExecuted, &rec.RollbackReason)
	if err != nil {
		if err == pgx.ErrNoRows {
			return actionRecord{}, false
		}
		t.Fatalf("query action for %s/%s: %v", category, objectID, err)
	}
	return rec, true
}

// indexAccessMethod returns the access method of a named index, or "" when
// the index does not exist. valid reports pg_index.indisvalid.
func indexAccessMethod(
	t *testing.T, pool *pgxpool.Pool, indexName string,
) (am string, valid bool, exists bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := pool.QueryRow(ctx,
		`SELECT am.amname, i.indisvalid
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indexrelid
		   JOIN pg_am am ON am.oid = c.relam
		  WHERE c.relname = $1`,
		indexName,
	).Scan(&am, &valid)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", false, false
		}
		t.Fatalf("indexAccessMethod %s: %v", indexName, err)
	}
	return am, valid, true
}

// scalarString runs a single-value query (e.g. SHOW work_mem).
func scalarString(t *testing.T, pool *pgxpool.Pool, sql string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var v string
	if err := pool.QueryRow(ctx, sql).Scan(&v); err != nil {
		t.Fatalf("scalar %q: %v", sql, err)
	}
	return v
}

// checkReport accumulates CHECK-xx lines so the run ends with the
// repo-mandated verification checklist.
type checkReport struct {
	lines []string
	fails int
}

func (r *checkReport) add(t *testing.T, id string, pass bool, desc string) {
	t.Helper()
	status := "PASS"
	if !pass {
		status = "FAIL"
		r.fails++
	}
	line := fmt.Sprintf("%s: [%s] %s", id, status, desc)
	r.lines = append(r.lines, line)
	t.Logf("%s", line)
	if !pass {
		t.Errorf("%s", line)
	}
}

func (r *checkReport) dump(t *testing.T, header string) {
	t.Helper()
	t.Logf("===== %s =====", header)
	for _, l := range r.lines {
		t.Logf("%s", l)
	}
	t.Logf("===== %d checks, %d failed =====", len(r.lines), r.fails)
}
