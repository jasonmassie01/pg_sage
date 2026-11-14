package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// cases-30 CHECK-01: GET /api/v1/cases for a fixture holding one finding
// with a pending action, one incident and one query hint is byte-identical
// to the response recorded on the unsplit cases_handlers.go
// (SAGE_UPDATE_CASES_SPLIT_GOLDEN=1 regenerates it). Run on PG14 and PG17.
// No concurrency or state tests here: TestCasesHandlerConcurrentRequests-
// ShareNoState covers concurrent reads; this test pins the projection.

const casesSplitGoldenPath = "testdata/cases_split_golden.json"

func seedSplitGoldenFixture(t *testing.T, ctx context.Context) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	statements := []string{
		`INSERT INTO sage.findings
		(id, created_at, last_seen, category, severity, object_type, object_identifier,
		 title, detail, recommendation, recommended_sql, rollback_sql, status)
		VALUES (9301, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z', 'missing_index',
		 'critical', 'table', 'public.orders', 'Seq scans on orders', '{"seq_scan":900}',
		 'Index it.', 'CREATE INDEX CONCURRENTLY idx_orders_c ON public.orders (c)',
		 'DROP INDEX CONCURRENTLY public.idx_orders_c', 'open')`,
		`INSERT INTO sage.action_queue
		(id, finding_id, proposed_sql, rollback_sql, action_risk, status, action_type,
		 proposed_at, expires_at, reason, policy_decision)
		VALUES (9401, 9301, 'CREATE INDEX CONCURRENTLY idx_orders_c ON public.orders (c)',
		 'DROP INDEX CONCURRENTLY public.idx_orders_c', 'moderate', 'pending',
		 'create_index', '2026-01-02T00:30:00Z', '2099-01-01T00:00:00Z',
		 'awaiting approval', 'approval_required')`,
		`INSERT INTO sage.incidents
		(id, detected_at, last_detected_at, severity, root_cause, source, database_name,
		 signal_ids, affected_objects, recommended_sql, action_risk, confidence)
		VALUES ('00000000-0000-0000-0000-000000009501', '2026-01-02T06:00:00Z',
		 '2026-01-02T06:30:00Z', 'warning', 'Idle-in-transaction PID 12345 blocks vacuum',
		 'deterministic', 'prod', '{vacuum_blocked}', '{public.orders}',
		 'SELECT pg_terminate_backend(12345)', 'high_risk', 0.8)`,
		`INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status, created_at, before_cost, after_cost)
		VALUES (9601, 'IndexScan(orders idx_orders_c)', 'seq_scan', 'active',
		 '2026-01-02T05:00:00Z', 120, 30)`,
	}
	for _, sql := range statements {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed split golden fixture: %v\n%s", err, sql)
		}
	}
}

func TestCasesHandlerSplitGoldenResponse(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetCaseTables(t, ctx)
	seedSplitGoldenFixture(t, ctx)
	cfg := caseAutonomousConfig()
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Pool: pool, Executor: caseGateExecutor(cfg),
	})
	rr := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/v1/cases?database=prod", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	got := utcTimestamps(t, rr.Body.Bytes())
	if os.Getenv("SAGE_UPDATE_CASES_SPLIT_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(casesSplitGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(casesSplitGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(casesSplitGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", casesSplitGoldenPath, err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(utcTimestamps(t, want))) {
		t.Fatalf("cases JSON changed:\n got %s\nwant %s", got, want)
	}
	// Today the pending action does not reach the case: the queue's ledger
	// read selects 27 columns and its scanner reads 35, so the finding case
	// is marked actions_unavailable (a defect reported in cases-30, fixed
	// separately; this golden pins the split, not that fix).
	for _, marker := range []string{`"total":3`, `"actions_unavailable":true`,
		`"incident:prod:vacuum_blocked:00000000-0000-0000-0000-000000009501"`,
		`"query_hint:prod:9601:`} {
		if !bytes.Contains(got, []byte(marker)) {
			t.Fatalf("golden response lacks %s: %.600s", marker, got)
		}
	}
}
