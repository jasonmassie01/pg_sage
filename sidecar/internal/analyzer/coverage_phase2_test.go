package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const phase2DSN = "postgres://postgres:postgres@localhost:5432/" +
	"postgres?sslmode=disable"

const destructiveTestLockKey = "pg_sage_test_cross_pkg"

// phase2Pool connects to local Postgres and bootstraps the sage schema.
// Skips the test if the database is unavailable.
func phase2Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, phase2DSN)
	if err != nil {
		t.Skipf("DB unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("DB ping failed: %v", err)
	}

	t.Cleanup(func() { pool.Close() })
	serializeAcrossPackages(t, context.Background(), pool)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Skipf("schema bootstrap failed: %v", err)
	}
	schema.ReleaseAdvisoryLock(ctx, pool)
	releasePoolAdvisoryLocks(ctx, pool)

	// Re-check while holding the cross-package lock.
	ensurePhase2SageSchema(t, pool)

	return pool
}

var phase2SageMu sync.Mutex

func serializeAcrossPackages(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn for cross-pkg lock: %v", err)
	}
	_, err = conn.Exec(ctx,
		"SELECT pg_advisory_lock(hashtext($1))",
		destructiveTestLockKey)
	if err != nil {
		conn.Release()
		t.Fatalf("acquire cross-package test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(),
			"SELECT pg_advisory_unlock(hashtext($1))",
			destructiveTestLockKey)
		conn.Release()
	})
}

// ensurePhase2SageSchema re-bootstraps the sage schema if it was
// dropped by concurrent schema package tests during go test ./...
func ensurePhase2SageSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	phase2SageMu.Lock()
	defer phase2SageMu.Unlock()
	ctx := context.Background()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM information_schema.tables
		 WHERE table_schema = 'sage'
		   AND table_name = 'findings'`).Scan(&n)
	if err == nil {
		return
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Skipf("re-bootstrap sage failed: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Skipf("re-migrate sage config: %v", err)
	}
	schema.ReleaseAdvisoryLock(ctx, pool)
	releasePoolAdvisoryLocks(ctx, pool)
}

func releasePoolAdvisoryLocks(ctx context.Context, pool *pgxpool.Pool) {
	total := int(pool.Stat().TotalConns())
	conns := make([]*pgxpool.Conn, 0, total)
	for i := 0; i < total; i++ {
		qctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		conn, err := pool.Acquire(qctx)
		cancel()
		if err != nil {
			break
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock_all()")
		conn.Release()
	}
}

// phase2ExecRetry runs sql and retries once after re-bootstrapping
// sage if the schema was dropped by a concurrent test package.
func phase2ExecRetry(
	t *testing.T, pool *pgxpool.Pool, sql string, args ...any,
) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, sql, args...)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, err = pool.Exec(ctx, sql, args...)
	}
	if err != nil {
		t.Fatalf("exec: %v\nsql: %s", err, sql)
	}
}

// phase2QueryRetry runs a query and retries once after
// re-bootstrapping sage if the schema was dropped mid-test.
func phase2QueryRetry(
	t *testing.T, pool *pgxpool.Pool, sql string, dest ...any,
) {
	t.Helper()
	ctx := context.Background()
	err := pool.QueryRow(ctx, sql).Scan(dest...)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		err = pool.QueryRow(ctx, sql).Scan(dest...)
	}
	if err != nil {
		t.Fatalf("query: %v\nsql: %s", err, sql)
	}
}

func phase2Config() *config.Config {
	return &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
			BatchSize:       100,
			MaxQueries:      100,
		},
		Analyzer: config.AnalyzerConfig{
			IntervalSeconds:              60,
			SlowQueryThresholdMs:         1000,
			TableBloatDeadTuplePct:       20,
			TableBloatMinRows:            1000,
			SeqScanMinRows:               10000,
			UnusedIndexWindowDays:        7,
			RegressionThresholdPct:       50,
			RegressionLookbackDays:       7,
			CacheHitRatioWarning:         0.95,
			XIDWraparoundWarning:         500000000,
			XIDWraparoundCritical:        1000000000,
			CheckpointFreqWarningPerHour: 6,
			IdleInTxTimeoutMinutes:       5,
		},
		Safety: config.SafetyConfig{
			CPUCeilingPct:           90,
			BackoffConsecutiveSkips: 5,
			QueryTimeoutMs:          500,
			DDLTimeoutSeconds:       30,
		},
	}
}

// phase2CleanFindings removes all open test findings from the DB.
// Must also clean alert_log first due to FK constraint.
func phase2CleanFindings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Clean alert_log rows that reference our test findings.
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.alert_log
		 WHERE finding_id IN (
		   SELECT id FROM sage.findings
		   WHERE category LIKE 'test_phase2_%'
		 )`)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, _ = pool.Exec(ctx,
			`DELETE FROM sage.alert_log
			 WHERE finding_id IN (
			   SELECT id FROM sage.findings
			   WHERE category LIKE 'test_phase2_%'
			 )`)
	}
	_, err = pool.Exec(ctx,
		`DELETE FROM sage.findings
		 WHERE category LIKE 'test_phase2_%'`)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, err = pool.Exec(ctx,
			`DELETE FROM sage.findings
			 WHERE category LIKE 'test_phase2_%'`)
	}
	if err != nil {
		t.Fatalf("cleanup findings: %v", err)
	}
}

// phase2CleanSnapshots removes test snapshots.
func phase2CleanSnapshots(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.snapshots
		 WHERE category = 'queries'
		   AND data::text LIKE '%phase2_test%'`)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, err = pool.Exec(ctx,
			`DELETE FROM sage.snapshots
			 WHERE category = 'queries'
			   AND data::text LIKE '%phase2_test%'`)
	}
	if err != nil {
		t.Fatalf("cleanup snapshots: %v", err)
	}
}

// phase2CleanExplainCache removes test explain_cache entries.
func phase2CleanExplainCache(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.explain_cache
		 WHERE query_text LIKE '%phase2_test%'`)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, err = pool.Exec(ctx,
			`DELETE FROM sage.explain_cache
			 WHERE query_text LIKE '%phase2_test%'`)
	}
	if err != nil {
		t.Fatalf("cleanup explain_cache: %v", err)
	}
}

// phase2CleanActionLog removes test action_log entries.
func phase2CleanActionLog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`DELETE FROM sage.action_log
		 WHERE sql_executed LIKE '%phase2_test%'`)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		ensurePhase2SageSchema(t, pool)
		_, err = pool.Exec(ctx,
			`DELETE FROM sage.action_log
			 WHERE sql_executed LIKE '%phase2_test%'`)
	}
	if err != nil {
		t.Fatalf("cleanup action_log: %v", err)
	}
}

// ---------------------------------------------------------------------------
// UpsertFindings
// ---------------------------------------------------------------------------

func TestPhase2_UpsertFindings_InsertNew(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	findings := []Finding{
		{
			Category:         "test_phase2_insert",
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: "public.users",
			Title:            "Test finding for upsert insert",
			Detail:           map[string]any{"key": "value"},
			Recommendation:   "Fix it",
			RecommendedSQL:   "ALTER TABLE users ...",
			RollbackSQL:      "ALTER TABLE users UNDO ...",
		},
	}

	err := UpsertFindings(ctx, pool, findings)
	if err != nil {
		t.Fatalf("UpsertFindings insert: %v", err)
	}

	// Verify the finding was inserted.
	var count int
	var severity, title string
	err = pool.QueryRow(ctx,
		`SELECT occurrence_count, severity, title
		 FROM sage.findings
		 WHERE category = 'test_phase2_insert'
		   AND object_identifier = 'public.users'
		   AND status = 'open'`,
	).Scan(&count, &severity, &title)
	if err != nil {
		t.Fatalf("query inserted finding: %v", err)
	}
	if count != 1 {
		t.Errorf("expected occurrence_count=1, got %d", count)
	}
	if severity != "warning" {
		t.Errorf("expected severity='warning', got %q", severity)
	}
	if title != "Test finding for upsert insert" {
		t.Errorf("expected matching title, got %q", title)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings WHERE category = 'test_phase2_insert'`)
}

func TestPhase2_UpsertFindings_BumpsExistingCount(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	f := Finding{
		Category:         "test_phase2_bump",
		Severity:         "warning",
		ObjectType:       "index",
		ObjectIdentifier: "idx_test",
		Title:            "Bump test",
		Detail:           map[string]any{"round": 1},
	}

	// First upsert — should insert.
	err := UpsertFindings(ctx, pool, []Finding{f})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Second upsert with updated severity — should bump count.
	f.Severity = "critical"
	f.Detail = map[string]any{"round": 2}
	err = UpsertFindings(ctx, pool, []Finding{f})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var count int
	var severity string
	err = pool.QueryRow(ctx,
		`SELECT occurrence_count, severity
		 FROM sage.findings
		 WHERE category = 'test_phase2_bump'
		   AND object_identifier = 'idx_test'
		   AND status = 'open'`,
	).Scan(&count, &severity)
	if err != nil {
		t.Fatalf("query bumped finding: %v", err)
	}
	if count != 2 {
		t.Errorf("expected occurrence_count=2, got %d", count)
	}
	if severity != "critical" {
		t.Errorf("expected updated severity 'critical', got %q", severity)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings WHERE category = 'test_phase2_bump'`)
}

func TestPhase2_UpsertFindings_EmptySlice(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()

	err := UpsertFindings(ctx, pool, nil)
	if err != nil {
		t.Fatalf("UpsertFindings with nil: %v", err)
	}

	err = UpsertFindings(ctx, pool, []Finding{})
	if err != nil {
		t.Fatalf("UpsertFindings with empty slice: %v", err)
	}
}

func TestPhase2_UpsertFindings_NilDetail(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	f := Finding{
		Category:         "test_phase2_nildetail",
		Severity:         "info",
		ObjectType:       "table",
		ObjectIdentifier: "t1",
		Title:            "nil detail test",
		Detail:           nil,
	}

	err := UpsertFindings(ctx, pool, []Finding{f})
	if err != nil {
		t.Fatalf("UpsertFindings with nil detail: %v", err)
	}

	// Verify it was stored.
	var count int
	err = pool.QueryRow(ctx,
		`SELECT occurrence_count FROM sage.findings
		 WHERE category = 'test_phase2_nildetail'
		   AND status = 'open'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query nil-detail finding: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1, got %d", count)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings
		 WHERE category = 'test_phase2_nildetail'`)
}

func TestPhase2_UpsertFindings_DoesNotReopenRecentlyResolvedAction(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()
	category := "test_phase2_recent_action"
	objectID := "public.child(parent_id)"
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings WHERE category = $1`, category)
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.action_log
		  WHERE sql_executed LIKE '%phase2_recent_action%'`)

	var actionLogID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.action_log
		    (action_type, finding_id, sql_executed, outcome)
		 VALUES ('create_index', 0, 'phase2_recent_action', 'success')
		 RETURNING id`,
	).Scan(&actionLogID)
	if err != nil {
		t.Fatalf("insert action log: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier, title,
		     detail, recommendation, recommended_sql, rollback_sql, status,
		     resolved_at, action_log_id)
		 VALUES ($1, 'warning', 'table', $2, 'recently resolved',
		     '{}'::jsonb, '', '', '', 'resolved', now(), $3)`,
		category, objectID, actionLogID,
	)
	if err != nil {
		t.Fatalf("insert resolved finding: %v", err)
	}

	err = UpsertFindings(ctx, pool, []Finding{{
		Category:         category,
		Severity:         "warning",
		ObjectType:       "table",
		ObjectIdentifier: objectID,
		Title:            "stale cycle should not reopen",
		Detail:           map[string]any{"constraint": "fk_child"},
		Recommendation:   "Create index",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY ON public.child (parent_id);",
	}})
	if err != nil {
		t.Fatalf("upsert stale finding: %v", err)
	}

	var openCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2
		    AND status = 'open'`,
		category, objectID,
	).Scan(&openCount)
	if err != nil {
		t.Fatalf("query open count: %v", err)
	}
	if openCount != 0 {
		t.Fatalf("recent action-resolved finding reopened; open count=%d",
			openCount)
	}

	_, err = pool.Exec(ctx,
		`UPDATE sage.findings
		    SET resolved_at = now() - interval '10 minutes'
		  WHERE category = $1 AND object_identifier = $2`,
		category, objectID,
	)
	if err != nil {
		t.Fatalf("age resolved finding: %v", err)
	}
	err = UpsertFindings(ctx, pool, []Finding{{
		Category:         category,
		Severity:         "warning",
		ObjectType:       "table",
		ObjectIdentifier: objectID,
		Title:            "old unresolved issue can reopen",
		Detail:           map[string]any{"constraint": "fk_child"},
		Recommendation:   "Create index",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY ON public.child (parent_id);",
	}})
	if err != nil {
		t.Fatalf("upsert after grace window: %v", err)
	}
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2
		    AND status = 'open'`,
		category, objectID,
	).Scan(&openCount)
	if err != nil {
		t.Fatalf("query reopened count: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("old resolved finding should reopen after grace; open count=%d",
			openCount)
	}

	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings WHERE category = $1`, category)
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.action_log
		  WHERE id = $1 OR sql_executed LIKE '%phase2_recent_action%'`,
		actionLogID)
}

// ---------------------------------------------------------------------------
// ResolveCleared
// ---------------------------------------------------------------------------

func TestPhase2_ResolveCleared_ResolvesInactive(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	// Insert two findings.
	findings := []Finding{
		{
			Category:         "test_phase2_resolve",
			Severity:         "warning",
			ObjectType:       "index",
			ObjectIdentifier: "idx_keep",
			Title:            "Keep me",
			Detail:           map[string]any{},
		},
		{
			Category:         "test_phase2_resolve",
			Severity:         "warning",
			ObjectType:       "index",
			ObjectIdentifier: "idx_remove",
			Title:            "Remove me",
			Detail:           map[string]any{},
		},
	}
	err := UpsertFindings(ctx, pool, findings)
	if err != nil {
		t.Fatalf("setup findings: %v", err)
	}

	// Only idx_keep is still active.
	active := map[string]bool{"idx_keep": true}
	err = ResolveCleared(ctx, pool, active, "test_phase2_resolve")
	if err != nil {
		t.Fatalf("ResolveCleared: %v", err)
	}

	// Verify idx_remove is resolved.
	var status string
	err = pool.QueryRow(ctx,
		`SELECT status FROM sage.findings
		 WHERE category = 'test_phase2_resolve'
		   AND object_identifier = 'idx_remove'`,
	).Scan(&status)
	if err != nil {
		t.Fatalf("query resolved finding: %v", err)
	}
	if status != "resolved" {
		t.Errorf("expected 'resolved', got %q", status)
	}

	// Verify idx_keep is still open.
	err = pool.QueryRow(ctx,
		`SELECT status FROM sage.findings
		 WHERE category = 'test_phase2_resolve'
		   AND object_identifier = 'idx_keep'`,
	).Scan(&status)
	if err != nil {
		t.Fatalf("query kept finding: %v", err)
	}
	if status != "open" {
		t.Errorf("expected 'open', got %q", status)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings
		 WHERE category = 'test_phase2_resolve'`)
}

func TestPhase2_ResolveCleared_AllActive(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	findings := []Finding{
		{
			Category:         "test_phase2_allactive",
			Severity:         "info",
			ObjectType:       "table",
			ObjectIdentifier: "t1",
			Title:            "Active 1",
			Detail:           map[string]any{},
		},
		{
			Category:         "test_phase2_allactive",
			Severity:         "info",
			ObjectType:       "table",
			ObjectIdentifier: "t2",
			Title:            "Active 2",
			Detail:           map[string]any{},
		},
	}
	err := UpsertFindings(ctx, pool, findings)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	active := map[string]bool{"t1": true, "t2": true}
	err = ResolveCleared(ctx, pool, active, "test_phase2_allactive")
	if err != nil {
		t.Fatalf("ResolveCleared: %v", err)
	}

	// Both should remain open.
	var openCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.findings
		 WHERE category = 'test_phase2_allactive'
		   AND status = 'open'`,
	).Scan(&openCount)
	if err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 2 {
		t.Errorf("expected 2 open, got %d", openCount)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings
		 WHERE category = 'test_phase2_allactive'`)
}

func TestPhase2_ResolveCleared_EmptyActive(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()

	findings := []Finding{
		{
			Category:         "test_phase2_emptyactive",
			Severity:         "info",
			ObjectType:       "table",
			ObjectIdentifier: "t1",
			Title:            "Should resolve",
			Detail:           map[string]any{},
		},
	}
	err := UpsertFindings(ctx, pool, findings)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Empty active set means everything should be resolved.
	active := map[string]bool{}
	err = ResolveCleared(ctx, pool, active, "test_phase2_emptyactive")
	if err != nil {
		t.Fatalf("ResolveCleared: %v", err)
	}

	var status string
	err = pool.QueryRow(ctx,
		`SELECT status FROM sage.findings
		 WHERE category = 'test_phase2_emptyactive'
		   AND object_identifier = 't1'`,
	).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "resolved" {
		t.Errorf("expected 'resolved', got %q", status)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.findings
		 WHERE category = 'test_phase2_emptyactive'`)
}

func TestPhase2_ResolveCleared_NoExistingFindings(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()

	// No findings exist for this category — should not error.
	active := map[string]bool{"anything": true}
	err := ResolveCleared(
		ctx, pool, active, "test_phase2_nonexistent_cat",
	)
	if err != nil {
		t.Fatalf("ResolveCleared on empty category: %v", err)
	}
}

// ---------------------------------------------------------------------------
// checkXIDWraparound (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_CheckXIDWraparound_Returns(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	findings := a.checkXIDWraparound(ctx)

	// On a healthy DB, we expect no findings (XID age should be well
	// below the warning threshold). The important thing is no error
	// and no panic.
	for _, f := range findings {
		if f.Category != "xid_wraparound" {
			t.Errorf(
				"unexpected category %q", f.Category,
			)
		}
		if f.Severity != "warning" && f.Severity != "critical" {
			t.Errorf(
				"unexpected severity %q for xid finding", f.Severity,
			)
		}
	}
}

func TestPhase2_CheckXIDWraparound_LowThreshold(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	// Set thresholds very low so the test DB triggers a warning.
	cfg.Analyzer.XIDWraparoundWarning = 1
	cfg.Analyzer.XIDWraparoundCritical = 1000000000

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	findings := a.checkXIDWraparound(ctx)
	if len(findings) == 0 {
		t.Error("expected at least one XID finding with low threshold")
	}
	found := false
	for _, f := range findings {
		if f.Category == "xid_wraparound" {
			found = true
			if f.Severity != "warning" && f.Severity != "critical" {
				t.Errorf("unexpected severity %q", f.Severity)
			}
		}
	}
	if !found {
		t.Error("no xid_wraparound finding with threshold=1")
	}
}

// ---------------------------------------------------------------------------
// checkConnectionLeaks (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_CheckConnectionLeaks_NoLeaks(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.IdleInTxTimeoutMinutes = 5

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	findings := a.checkConnectionLeaks(ctx)
	// On a clean test DB, there should be no idle-in-transaction
	// connections lasting > 5 minutes.
	for _, f := range findings {
		if f.Category != "connection_leak" {
			t.Errorf("unexpected category %q", f.Category)
		}
	}
}

func TestPhase2_CheckConnectionLeaks_ZeroTimeout(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	// A zero timeout means the query uses make_interval(mins => 0),
	// which should return connections idle for > 0 minutes.
	cfg.Analyzer.IdleInTxTimeoutMinutes = 0

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	// Should not error even with zero timeout.
	findings := a.checkConnectionLeaks(ctx)
	// We can't guarantee findings exist, but the function should not
	// panic or error.
	_ = findings
}

// ---------------------------------------------------------------------------
// buildHistoricalAverages (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_BuildHistoricalAverages_Empty(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanSnapshots(t, pool)
	cfg := phase2Config()

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	avgs := a.buildHistoricalAverages(ctx)
	// With no snapshots, we get an empty (or nil) map.
	if len(avgs) > 0 {
		// May have pre-existing snapshots from other tests. That's OK.
		t.Logf("found %d averages (may be from prior test data)", len(avgs))
	}
}

func TestPhase2_BuildHistoricalAverages_WithData(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanSnapshots(t, pool)
	ctx := context.Background()
	cfg := phase2Config()

	// Insert test snapshot data.
	type queryEntry struct {
		QueryID        int64   `json:"queryid"`
		MeanExecTimeMs float64 `json:"mean_exec_time_ms"`
		Phase2Test     string  `json:"phase2_test"`
	}

	snap1 := []queryEntry{
		{QueryID: 111, MeanExecTimeMs: 10.0, Phase2Test: "phase2_test"},
		{QueryID: 222, MeanExecTimeMs: 20.0, Phase2Test: "phase2_test"},
	}
	snap2 := []queryEntry{
		{QueryID: 111, MeanExecTimeMs: 30.0, Phase2Test: "phase2_test"},
		{QueryID: 222, MeanExecTimeMs: 40.0, Phase2Test: "phase2_test"},
	}

	for _, s := range [][]queryEntry{snap1, snap2} {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO sage.snapshots (category, data)
			 VALUES ('queries', $1)`, data)
		if err != nil {
			t.Fatalf("insert snapshot: %v", err)
		}
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	avgs := a.buildHistoricalAverages(ctx)

	if avgs == nil {
		t.Fatal("expected non-nil averages map")
	}

	// QueryID 111: avg of 10 and 30 = 20.0
	if avg, ok := avgs[111]; !ok {
		t.Error("missing average for queryid 111")
	} else if avg < 19.9 || avg > 20.1 {
		t.Errorf("expected ~20.0 for queryid 111, got %f", avg)
	}

	// QueryID 222: avg of 20 and 40 = 30.0
	if avg, ok := avgs[222]; !ok {
		t.Error("missing average for queryid 222")
	} else if avg < 29.9 || avg > 30.1 {
		t.Errorf("expected ~30.0 for queryid 222, got %f", avg)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.snapshots
		 WHERE category = 'queries'
		   AND data::text LIKE '%phase2_test%'`)
}

// ---------------------------------------------------------------------------
// loadRecentlyCreatedIndexes (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_LoadRecentlyCreatedIndexes_Empty(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanActionLog(t, pool)
	cfg := phase2Config()

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	a.loadRecentlyCreatedIndexes(ctx)
	// Should not panic or error. Map should be empty or very small
	// depending on prior test state.
	if a.extras.RecentlyCreated == nil {
		t.Error("RecentlyCreated should be non-nil (even if empty)")
	}
}

func TestPhase2_LoadRecentlyCreatedIndexes_FindsRecent(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanActionLog(t, pool)
	ctx := context.Background()
	cfg := phase2Config()

	// Insert a recent CREATE INDEX action.
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, sql_executed, outcome, executed_at)
		 VALUES ('create_index',
		         'CREATE INDEX phase2_test_idx ON public.users (email)',
		         'success', now())`)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.loadRecentlyCreatedIndexes(ctx)

	if _, ok := a.extras.RecentlyCreated["phase2_test_idx"]; !ok {
		t.Errorf(
			"expected 'phase2_test_idx' in RecentlyCreated, got %v",
			a.extras.RecentlyCreated,
		)
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.action_log
		 WHERE sql_executed LIKE '%phase2_test%'`)
}

func TestPhase2_LoadRecentlyCreatedIndexes_IgnoresOld(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanActionLog(t, pool)
	ctx := context.Background()
	cfg := phase2Config()
	cfg.Analyzer.UnusedIndexWindowDays = 1

	// Insert an old CREATE INDEX action (30 days ago).
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, sql_executed, outcome, executed_at)
		 VALUES ('create_index',
		         'CREATE INDEX phase2_test_old_idx ON public.orders (id)',
		         'success', now() - interval '30 days')`)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.loadRecentlyCreatedIndexes(ctx)

	if _, ok := a.extras.RecentlyCreated["phase2_test_old_idx"]; ok {
		t.Error("old index should not appear in RecentlyCreated")
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.action_log
		 WHERE sql_executed LIKE '%phase2_test%'`)
}

func TestPhase2_LoadRecentlyCreatedIndexes_IgnoresFailed(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanActionLog(t, pool)
	ctx := context.Background()
	cfg := phase2Config()

	// Insert a failed CREATE INDEX action.
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.action_log
		 (action_type, sql_executed, outcome, executed_at)
		 VALUES ('create_index',
		         'CREATE INDEX phase2_test_fail_idx ON public.t (x)',
		         'failure', now())`)
	if err != nil {
		t.Fatalf("insert action_log: %v", err)
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.loadRecentlyCreatedIndexes(ctx)

	if _, ok := a.extras.RecentlyCreated["phase2_test_fail_idx"]; ok {
		t.Error("failed index should not appear in RecentlyCreated")
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.action_log
		 WHERE sql_executed LIKE '%phase2_test%'`)
}

// ---------------------------------------------------------------------------
// checkSortWithoutIndex (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_CheckSortWithoutIndex_NoEntries(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanExplainCache(t, pool)
	cfg := phase2Config()

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	findings := a.checkSortWithoutIndex(ctx)
	// With no explain_cache entries, should return nil/empty.
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestPhase2_CheckSortWithoutIndex_DetectsPattern(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanExplainCache(t, pool)
	ctx := context.Background()
	cfg := phase2Config()

	// Insert an explain_cache entry with a Sort+Limit pattern.
	planJSON := `[{"Plan": {
		"Node Type": "Limit",
		"Plan Rows": 10,
		"Plans": [{
			"Node Type": "Sort",
			"Plan Rows": 100000,
			"Sort Key": ["orders.created_at DESC"]
		}]
	}}]`

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.explain_cache
		 (queryid, query_text, plan_json, source, total_cost)
		 VALUES (99901,
		         'SELECT phase2_test FROM orders ORDER BY created_at DESC LIMIT 10',
		         $1, 'autoexplain', 1000.0)`,
		planJSON)
	if err != nil {
		t.Fatalf("insert explain_cache: %v", err)
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	findings := a.checkSortWithoutIndex(ctx)

	found := false
	for _, f := range findings {
		if f.Category == "sort_without_index" {
			found = true
			if f.ObjectIdentifier != "queryid:99901" {
				t.Errorf(
					"expected 'queryid:99901', got %q",
					f.ObjectIdentifier,
				)
			}
			detail, ok := f.Detail["ratio"].(float64)
			if !ok || detail < 9999 {
				t.Errorf(
					"expected high ratio, got %v", f.Detail["ratio"],
				)
			}
		}
	}
	if !found {
		t.Error("expected sort_without_index finding from explain_cache")
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.explain_cache
		 WHERE query_text LIKE '%phase2_test%'`)
}

// ---------------------------------------------------------------------------
// checkPlanRegression (DB integration)
// ---------------------------------------------------------------------------

func TestPhase2_CheckPlanRegression_NoData(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanExplainCache(t, pool)
	cfg := phase2Config()

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()

	findings := a.checkPlanRegression(ctx)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestPhase2_CheckPlanRegression_DetectsRegression(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanExplainCache(t, pool)
	ctx := context.Background()
	cfg := phase2Config()

	// Previous plan: low cost, Index Scan.
	prevPlan := `[{"Plan": {
		"Node Type": "Index Scan",
		"Plan Rows": 100
	}}]`

	// Current plan: high cost, Seq Scan (10x regression).
	curPlan := `[{"Plan": {
		"Node Type": "Seq Scan",
		"Plan Rows": 100000
	}}]`

	// Insert old plan (captured_at 2 days ago).
	_, err := pool.Exec(ctx,
		`INSERT INTO sage.explain_cache
		 (queryid, query_text, plan_json, source, total_cost,
		  execution_time, captured_at)
		 VALUES (99902,
		         'SELECT phase2_test FROM big_table WHERE id > 0',
		         $1, 'autoexplain', 10.0, 5.0,
		         now() - interval '2 days')`,
		prevPlan)
	if err != nil {
		t.Fatalf("insert prev plan: %v", err)
	}

	// Insert current plan (captured now).
	_, err = pool.Exec(ctx,
		`INSERT INTO sage.explain_cache
		 (queryid, query_text, plan_json, source, total_cost,
		  execution_time, captured_at)
		 VALUES (99902,
		         'SELECT phase2_test FROM big_table WHERE id > 0',
		         $1, 'autoexplain', 100.0, 50.0, now())`,
		curPlan)
	if err != nil {
		t.Fatalf("insert current plan: %v", err)
	}

	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	findings := a.checkPlanRegression(ctx)

	found := false
	for _, f := range findings {
		if f.Category == "plan_regression" {
			found = true
			costRatio, ok := f.Detail["cost_ratio"].(float64)
			if !ok {
				t.Error("cost_ratio not in detail")
			} else if costRatio < 9.0 {
				t.Errorf("expected ~10x cost ratio, got %f", costRatio)
			}
		}
	}
	if !found {
		t.Error("expected plan_regression finding")
	}

	// Cleanup.
	_, _ = pool.Exec(ctx,
		`DELETE FROM sage.explain_cache
		 WHERE query_text LIKE '%phase2_test%'`)
}

// ---------------------------------------------------------------------------
// Run (integration — verifies loop starts and stops)
// ---------------------------------------------------------------------------

func phase2Collector() *collector.Collector {
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 60,
			BatchSize:       100,
			MaxQueries:      100,
		},
		Safety: config.SafetyConfig{
			CPUCeilingPct:           90,
			BackoffConsecutiveSkips: 5,
		},
	}
	return collector.New(nil, cfg, 160000, noopLog)
}

func TestPhase2_Run_CancelsCleanly(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.IntervalSeconds = 1

	coll := phase2Collector()

	a := New(pool, cfg, coll, nil, nil, nil, nil, noopLog)
	ctx, cancel := context.WithTimeout(
		context.Background(), 200*time.Millisecond,
	)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Run exited cleanly.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// safeRatio (unit tests for partial coverage)
// ---------------------------------------------------------------------------

func TestPhase2_SafeRatio_ZeroDenominator(t *testing.T) {
	got := safeRatio(100, 0)
	if got != 0 {
		t.Errorf("expected 0 for zero denominator, got %f", got)
	}
}

func TestPhase2_SafeRatio_NegativeDenominator(t *testing.T) {
	got := safeRatio(100, -5)
	if got != 0 {
		t.Errorf("expected 0 for negative denominator, got %f", got)
	}
}

func TestPhase2_SafeRatio_Normal(t *testing.T) {
	got := safeRatio(100, 10)
	if got != 10.0 {
		t.Errorf("expected 10.0, got %f", got)
	}
}

func TestPhase2_SafeRatio_ZeroNumerator(t *testing.T) {
	got := safeRatio(0, 10)
	if got != 0 {
		t.Errorf("expected 0 for zero numerator, got %f", got)
	}
}

func TestPhase2_SafeRatio_BothZero(t *testing.T) {
	got := safeRatio(0, 0)
	if got != 0 {
		t.Errorf("expected 0 for both zero, got %f", got)
	}
}

// ---------------------------------------------------------------------------
// buildSortFinding (unit tests for partial coverage)
// ---------------------------------------------------------------------------

func TestPhase2_BuildSortFinding_WithSortKey(t *testing.T) {
	entry := ExplainEntry{
		QueryID:   42,
		QueryText: "SELECT * FROM orders ORDER BY created_at LIMIT 5",
	}
	f := buildSortFinding(entry, 50000, 5, []string{"orders.created_at DESC"})
	if f == nil {
		t.Fatal("expected non-nil finding")
	}
	if f.Category != "sort_without_index" {
		t.Errorf("expected 'sort_without_index', got %q", f.Category)
	}
	if f.ObjectIdentifier != "queryid:42" {
		t.Errorf("expected 'queryid:42', got %q", f.ObjectIdentifier)
	}
	cols, ok := f.Detail["sort_columns"].([]string)
	if !ok || len(cols) != 1 {
		t.Fatalf("expected sort_columns with 1 entry, got %v", f.Detail["sort_columns"])
	}
	if cols[0] != "created_at" {
		t.Errorf("expected 'created_at', got %q", cols[0])
	}
	if f.Detail["ratio"] != float64(10000) {
		t.Errorf("expected ratio=10000, got %v", f.Detail["ratio"])
	}
}

func TestPhase2_BuildSortFinding_EmptySortKey(t *testing.T) {
	entry := ExplainEntry{
		QueryID:   43,
		QueryText: "SELECT * FROM t ORDER BY x LIMIT 1",
	}
	f := buildSortFinding(entry, 1000, 1, nil)
	if f == nil {
		t.Fatal("expected non-nil finding")
	}
	if f.Recommendation != "Add an index matching the ORDER BY columns to avoid sorting." {
		t.Errorf("unexpected recommendation: %q", f.Recommendation)
	}
	// Should not have sort_columns in detail.
	if _, exists := f.Detail["sort_columns"]; exists {
		t.Error("expected no sort_columns with nil sortKey")
	}
}

func TestPhase2_BuildSortFinding_MultipleSortKeys(t *testing.T) {
	entry := ExplainEntry{
		QueryID:   44,
		QueryText: "SELECT * FROM t ORDER BY a, b LIMIT 10",
	}
	f := buildSortFinding(
		entry, 10000, 10,
		[]string{"t.a ASC NULLS LAST", "t.b DESC"},
	)
	if f == nil {
		t.Fatal("expected non-nil finding")
	}
	cols, ok := f.Detail["sort_columns"].([]string)
	if !ok || len(cols) != 2 {
		t.Fatalf("expected 2 sort columns, got %v", f.Detail["sort_columns"])
	}
	if cols[0] != "a" {
		t.Errorf("expected 'a', got %q", cols[0])
	}
	if cols[1] != "b" {
		t.Errorf("expected 'b', got %q", cols[1])
	}
}

func TestPhase2_BuildSortFinding_ZeroLimitRows(t *testing.T) {
	entry := ExplainEntry{QueryID: 45, QueryText: "q"}
	f := buildSortFinding(entry, 100, 0, nil)
	if f == nil {
		t.Fatal("expected non-nil finding")
	}
	// safeRatio(100, 0) = 0, so ratio in detail should be 0.
	ratio, ok := f.Detail["ratio"].(float64)
	if !ok || ratio != 0 {
		t.Errorf("expected ratio=0 for zero limitRows, got %v", f.Detail["ratio"])
	}
}

// ---------------------------------------------------------------------------
// containsChange (unit tests for partial coverage)
// ---------------------------------------------------------------------------

func TestPhase2_ContainsChange_Found(t *testing.T) {
	changes := []string{
		"Index Scan \u2192 Seq Scan",
		"Nested Loop \u2192 Hash Join",
	}
	if !containsChange(changes, "Index Scan") {
		t.Error("expected true for 'Index Scan'")
	}
	if !containsChange(changes, "Seq Scan") {
		t.Error("expected true for 'Seq Scan'")
	}
}

func TestPhase2_ContainsChange_NotFound(t *testing.T) {
	changes := []string{"Index Scan \u2192 Seq Scan"}
	if containsChange(changes, "Bitmap Scan") {
		t.Error("expected false for 'Bitmap Scan'")
	}
}

func TestPhase2_ContainsChange_EmptySlice(t *testing.T) {
	if containsChange(nil, "anything") {
		t.Error("expected false for nil slice")
	}
	if containsChange([]string{}, "anything") {
		t.Error("expected false for empty slice")
	}
}

// ---------------------------------------------------------------------------
// hasDiskSpillForNode (unit tests for partial coverage)
// ---------------------------------------------------------------------------

func TestPhase2_HasDiskSpillForNode_NotSort(t *testing.T) {
	// Non-Sort nodes should always return false.
	if hasDiskSpillForNode("Seq Scan", []byte(`[{"Plan":{"Node Type":"Seq Scan"}}]`)) {
		t.Error("expected false for non-Sort node")
	}
}

func TestPhase2_HasDiskSpillForNode_SortWithDisk(t *testing.T) {
	planJSON := `[{"Plan": {
		"Node Type": "Sort",
		"Sort Space Type": "Disk",
		"Plans": []
	}}]`
	if !hasDiskSpillForNode("Sort", []byte(planJSON)) {
		t.Error("expected true for Sort with Disk spill")
	}
}

func TestPhase2_HasDiskSpillForNode_SortWithoutDisk(t *testing.T) {
	planJSON := `[{"Plan": {
		"Node Type": "Sort",
		"Sort Space Type": "Memory",
		"Plans": []
	}}]`
	if hasDiskSpillForNode("Sort", []byte(planJSON)) {
		t.Error("expected false for Sort with Memory")
	}
}

func TestPhase2_HasDiskSpillForNode_InvalidJSON(t *testing.T) {
	if hasDiskSpillForNode("Sort", []byte(`invalid json`)) {
		t.Error("expected false for invalid JSON")
	}
}

func TestPhase2_HasDiskSpillForNode_EmptyWrapper(t *testing.T) {
	if hasDiskSpillForNode("Sort", []byte(`[]`)) {
		t.Error("expected false for empty wrapper")
	}
}

func TestPhase2_HasDiskSpillForNode_NestedSortDisk(t *testing.T) {
	planJSON := `[{"Plan": {
		"Node Type": "Limit",
		"Plans": [{
			"Node Type": "Sort",
			"Sort Space Type": "Disk"
		}]
	}}]`
	if !hasDiskSpillForNode("Sort", []byte(planJSON)) {
		t.Error("expected true for nested Sort with Disk")
	}
}

// ---------------------------------------------------------------------------
// cycle (integration — exercises the full analysis loop)
// ---------------------------------------------------------------------------

func TestPhase2_Cycle_NilSnapshot(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.IntervalSeconds = 60

	coll := phase2Collector()

	var logged []string
	logFn := func(level string, msg string, args ...any) {
		logged = append(logged,
			fmt.Sprintf("[%s] %s", level, fmt.Sprintf(msg, args...)))
	}

	a := New(pool, cfg, coll, nil, nil, nil, nil, logFn)
	ctx := context.Background()

	a.cycle(ctx)

	// With nil snapshot, cycle should log "no snapshot yet" and exit.
	foundSkip := false
	for _, msg := range logged {
		if len(msg) > 0 && contains(msg, "no snapshot yet") {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf(
			"expected 'no snapshot yet' log, got %v", logged,
		)
	}
}

// contains checks if substr is in s. Simple helper to avoid
// importing strings in the test file (already imported in production).
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPhase2_Cycle_RunWithRealCollector(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.IntervalSeconds = 1
	// Use a 1-second collector interval so the first tick fires fast.
	cfg.Collector.IntervalSeconds = 1

	// Detect the actual PG version to use the correct collector queries.
	var pgVersionStr string
	err := pool.QueryRow(context.Background(),
		"SHOW server_version_num").Scan(&pgVersionStr)
	if err != nil {
		t.Skipf("cannot detect PG version: %v", err)
	}
	var pgVersionNum int
	fmt.Sscanf(pgVersionStr, "%d", &pgVersionNum)
	t.Logf("detected PG version: %d", pgVersionNum)

	// Create a real collector wired to the test DB.
	coll := collector.New(pool, cfg, pgVersionNum, noopLog)

	// Trigger a collection by running the collector briefly.
	collCtx, collCancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer collCancel()
	go coll.Run(collCtx)

	// Wait for a snapshot to appear (collector ticks at 1s).
	var snap *collector.Snapshot
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		snap = coll.LatestSnapshot()
		if snap != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	collCancel()

	if snap == nil {
		t.Skip("collector did not produce a snapshot in time")
	}

	var findingCount int
	logFn := func(level string, msg string, args ...any) {
		formatted := fmt.Sprintf(msg, args...)
		if level == "INFO" && contains(formatted, "analyzer cycle:") {
			fmt.Sscanf(
				formatted,
				"analyzer cycle: %d findings",
				&findingCount,
			)
		}
	}

	a := New(pool, cfg, coll, nil, nil, nil, nil, logFn)
	ctx := context.Background()

	a.cycle(ctx)

	// The cycle completed. We verify that findings were set.
	findings := a.LatestFindings()
	t.Logf("cycle produced %d findings", len(findings))
	// findingCount from log should match.
	if findingCount != len(findings) {
		t.Errorf(
			"log reported %d findings but LatestFindings has %d",
			findingCount, len(findings),
		)
	}
}

// ---------------------------------------------------------------------------
// Downsample (unit test — edge cases)
// ---------------------------------------------------------------------------

func TestPhase2_Downsample_LessThanMax(t *testing.T) {
	items := []int{1, 2, 3}
	result := downsample(items, 10)
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}
}

func TestPhase2_Downsample_ExactMax(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	result := downsample(items, 5)
	if len(result) != 5 {
		t.Errorf("expected 5, got %d", len(result))
	}
}

func TestPhase2_Downsample_MoreThanMax(t *testing.T) {
	items := make([]int, 200)
	for i := range items {
		items[i] = i
	}
	result := downsample(items, 50)
	if len(result) != 50 {
		t.Errorf("expected 50, got %d", len(result))
	}
	// First item should be items[0].
	if result[0] != 0 {
		t.Errorf("expected first item 0, got %d", result[0])
	}
}

func TestPhase2_Downsample_Empty(t *testing.T) {
	result := downsample([]int{}, 10)
	if len(result) != 0 {
		t.Errorf("expected 0, got %d", len(result))
	}
}

func TestPhase2_Downsample_MaxOne(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	result := downsample(items, 1)
	if len(result) != 1 {
		t.Errorf("expected 1, got %d", len(result))
	}
	if result[0] != 1 {
		t.Errorf("expected first item, got %d", result[0])
	}
}
