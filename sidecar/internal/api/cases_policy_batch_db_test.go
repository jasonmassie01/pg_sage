package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tracedCasePool is a pool on the test database whose every statement is
// recorded, standing in for the monitored database's pool.
func tracedCasePool(t *testing.T) (*pgxpool.Pool, *testdb.QueryRecorder, context.Context) {
	t.Helper()
	_, ctx := phase2RequireDB(t)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, rec, ctx
}

func resetCaseTables(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.incidents"); err != nil {
		t.Fatalf("clean incidents: %v", err)
	}
}

// dbCaseManager registers "prod" on pool with an executor whose standing
// gate reads the emergency stop, the policy and the budget usage from it.
func dbCaseManager(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *fleet.DatabaseManager {
	t.Helper()
	cfg := caseAutonomousConfig()
	exec := executor.New(pool, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	if err := exec.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatalf("enable standing policy: %v", err)
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Pool: pool, Executor: exec,
	})
	return mgr
}

// seedCountFindings inserts n open findings over three action types and
// two open incidents with a backend-signal candidate.
func seedCountFindings(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		SELECT 'stale_statistics', 'warning', 'table', 'public.t' || g,
		       'finding ' || g, '{}'::jsonb, 'act',
		       CASE g % 3 WHEN 0 THEN 'ANALYZE public.t' || g
		                  WHEN 1 THEN 'VACUUM public.t' || g
		                  ELSE 'CREATE INDEX CONCURRENTLY i' || g || ' ON public.t' || g || ' (c)'
		       END, 'open'
		  FROM generate_series(1, $1) AS g`, n)
	if err != nil {
		t.Fatalf("seed %d findings: %v", n, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, database_name, signal_ids, recommended_sql, action_risk)
		VALUES ('warning', 'Idle-in-transaction PID 12345 blocks vacuum', 'deterministic', 'prod',
		        '{vacuum_blocked}', 'SELECT pg_terminate_backend(12345)', 'high_risk'),
		       ('critical', 'lock chain', 'deterministic', 'prod', '{lock_chain}',
		        'SELECT pg_cancel_backend(4242)', 'moderate')`)
	if err != nil {
		t.Fatalf("seed incidents: %v", err)
	}
}

type policyPathCount struct{ emergencyStop, policyDoc, usage, inFlight, total int }

func countPolicyPath(rec *testdb.QueryRecorder) policyPathCount {
	return policyPathCount{
		emergencyStop: len(rec.Matching("sage.config", "emergency_stop")),
		policyDoc:     len(rec.Matching("FROM sage.policy")),
		usage:         len(rec.Matching("JOIN sage.decision d ON d.id = al.decision_id")),
		inFlight:      len(rec.Matching("al.id, 0::bigint, al.sql_executed")),
		total:         rec.RoundTrips(),
	}
}

// hygieneFindings is how many of seedCountFindings' n findings ANALYZE or
// VACUUM (g % 3 != 2): self-initiated hygiene the gate reaches the budget
// for; the CREATE INDEX findings stop at approval first.
func hygieneFindings(n int) int {
	count := 0
	for g := 1; g <= n; g++ {
		if g%3 != 2 {
			count++
		}
	}
	return count
}

// CHECK-02, as cases-04 narrows it: the emergency stop and the policy are
// read once per database for 50 and for 500 findings. What a finding's
// own table decides (CHECK-08) is read per finding and nothing more: one
// in-flight verification read per finding statement, one usage read per
// statement that reaches its budget; every other statement is constant.
// (cases-06 pinned the whole count constant while verdicts were
// family-only; that was the bug cases-04 fixes.)
func TestCasesPolicyStatementsScaleOnlyWithFindingTargets(t *testing.T) {
	pool, rec, ctx := tracedCasePool(t)
	counts := map[int]policyPathCount{}
	for _, n := range []int{50, 500} {
		resetCaseTables(t, ctx)
		mgr := dbCaseManager(t, ctx, pool)
		seedCountFindings(t, ctx, n)
		rec.Reset()
		got, err := queryProjectedCases(ctx, mgr, "prod")
		if err != nil {
			t.Fatalf("queryProjectedCases(%d): %v", n, err)
		}
		if len(got) != n+2 {
			t.Fatalf("cases = %d, want %d findings + 2 incidents", len(got), n)
		}
		counts[n] = countPolicyPath(rec)
		t.Logf("%d findings: %+v", n, counts[n])
		c := counts[n]
		if c.emergencyStop != 1 || c.policyDoc != 1 {
			t.Fatalf("%d findings: emergency-stop / policy reads = %d / %d, want 1 / 1",
				n, c.emergencyStop, c.policyDoc)
		}
		if c.inFlight != n || c.usage != hygieneFindings(n) {
			t.Fatalf("%d findings: in-flight / usage reads = %d / %d, want %d / %d", n,
				c.inFlight, c.usage, n, hygieneFindings(n))
		}
	}
	small, large := counts[50], counts[500]
	if fixed := small.total - small.usage - small.inFlight; fixed !=
		large.total-large.usage-large.inFlight {
		t.Fatalf("statements besides per-target reads = %d / %d, want the same", fixed,
			large.total-large.usage-large.inFlight)
	}
}

// Two dashboards polling /api/v1/cases at once get the same answer (run
// under -race where cgo is available).
func TestCasesHandlerConcurrentRequestsShareNoState(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr := dbCaseManager(t, ctx, pool)
	seedCountFindings(t, ctx, 60)
	handler := casesHandler(mgr)
	bodies := make([][]byte, 2)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/cases?database=prod", nil))
			codes[i], bodies[i] = rr.Code, rr.Body.Bytes()
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status = %d: %s", i, code, bodies[i])
		}
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("concurrent responses differ:\n%s\n%s", bodies[0], bodies[1])
	}
	if !bytes.Contains(bodies[0], []byte(`"total":62`)) {
		t.Fatalf("response total is not 62: %.300s", bodies[0])
	}
}

const casesGoldenPath = "testdata/cases_policy_golden.json"

// seedGoldenFixture is a fixed set of findings and incidents with explicit
// ids and timestamps, so the cases JSON is reproducible.
func seedGoldenFixture(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(id, created_at, last_seen, category, severity, object_type, object_identifier,
		 title, detail, recommendation, recommended_sql, rollback_sql, status)
		VALUES
		(9001, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 'stale_statistics', 'warning',
		 'table', 'public.orders', 'Stale stats on orders', '{"n_mod":12000}', 'Analyze it.',
		 'ANALYZE public.orders', NULL, 'open'),
		(9002, '2026-01-01T00:00:00Z', '2026-01-02T01:00:00Z', 'missing_index', 'critical',
		 'table', 'public.orders', 'Seq scans on orders', '{"seq_scan":900}', 'Index it.',
		 'CREATE INDEX CONCURRENTLY idx_orders_c ON public.orders (c)',
		 'DROP INDEX CONCURRENTLY public.idx_orders_c', 'open'),
		(9003, '2026-01-01T00:00:00Z', '2026-01-02T02:00:00Z', 'table_bloat', 'warning',
		 'table', 'public.events', 'Dead tuples on events', '{"dead":50000}', 'Vacuum it.',
		 'VACUUM public.events', NULL, 'open'),
		(9004, '2026-01-01T00:00:00Z', '2026-01-02T03:00:00Z', 'config', 'info',
		 'database', 'prod', 'work_mem is low', '{}', 'Raise work_mem.',
		 'ALTER SYSTEM SET work_mem = ''64MB''', 'ALTER SYSTEM RESET work_mem', 'open'),
		(9005, '2026-01-01T00:00:00Z', '2026-01-02T04:00:00Z', 'unused_index', 'info',
		 'index', 'public.idx_old', 'Unused index idx_old', '{"idx_scan":0}', 'Drop it.',
		 'DROP INDEX CONCURRENTLY public.idx_old', NULL, 'open'),
		(9006, '2026-01-01T00:00:00Z', '2026-01-02T05:00:00Z', 'slow_query', 'warning',
		 'query', 'queryid:77', 'Slow report query', '{"query":"SELECT * FROM public.r"}',
		 'Review the plan.', NULL, NULL, 'open')`)
	if err != nil {
		t.Fatalf("seed golden findings: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.incidents
		(id, detected_at, last_detected_at, severity, root_cause, source, database_name,
		 signal_ids, affected_objects, recommended_sql, action_risk, confidence)
		VALUES
		('00000000-0000-0000-0000-000000009101', '2026-01-02T06:00:00Z',
		 '2026-01-02T06:30:00Z', 'warning', 'Idle-in-transaction PID 12345 blocks vacuum',
		 'deterministic', 'prod', '{vacuum_blocked}', '{public.orders}',
		 'SELECT pg_terminate_backend(12345)', 'high_risk', 0.8),
		('00000000-0000-0000-0000-000000009102', '2026-01-02T07:00:00Z',
		 '2026-01-02T07:00:00Z', 'critical', 'Connections near max_connections',
		 'deterministic', 'prod', '{connections_high}', '{}', NULL, NULL, 0.9)`)
	if err != nil {
		t.Fatalf("seed golden incidents: %v", err)
	}
}

// CHECK-03: /api/v1/cases for a fixed fixture is byte-identical to the
// recorded response (SAGE_UPDATE_CASES_GOLDEN=1 regenerates it). cases-04
// regenerated it: the ALTER SYSTEM case shows alter_system_guc, and the
// missing-index case the executor's approval_required.
// The T1 integration regenerated it: the incident diagnostics propose their
// probes' display SQL (playbooks-04), timestamps are UTC. cases-10
// regenerated it: the fixture's evidence (January 2026) is past every
// candidate's expires_at, so each candidate is blocked evidence_expired;
// the policy's own reason stays in policy_decision. lab-03a regenerated it:
// the unused-index drop has no clone rehearsal, so the policy queues it for
// approval (the verification ladder's entry rule).
func TestCasesHandlerGoldenResponse(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedGoldenFixture(t, ctx)
	cfg := caseAutonomousConfig()
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Pool: pool, Executor: caseGateExecutor(cfg),
	})
	rr := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/cases?database=prod", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	// Instants in UTC: the response renders them in the host's zone.
	got := utcTimestamps(t, rr.Body.Bytes())
	if os.Getenv("SAGE_UPDATE_CASES_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(casesGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(casesGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(casesGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", casesGoldenPath, err)
	}
	// Trimmed: a CRLF checkout must not turn the trailing newline into a diff.
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(utcTimestamps(t, want))) {
		t.Fatalf("cases JSON changed:\n got %s\nwant %s", got, want)
	}
	if !bytes.Contains(got, []byte(`"total":8`)) {
		t.Fatalf("golden fixture should project 8 cases: %.300s", got)
	}
}
