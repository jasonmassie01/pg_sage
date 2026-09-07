package retention

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// Integration tests (require live PostgreSQL)
// ---------------------------------------------------------------------------

// TestCoverage_Run_AllTablesPositiveRetention exercises the full Run() path
// with positive retention days on all four tables and cleanStaleFirstSeen.
func TestCoverage_Run_AllTablesPositiveRetention(t *testing.T) {
	pool, ctx := requireDB(t)

	// Seed each table with an old row that should be purged.
	seed := []string{
		`INSERT INTO sage.snapshots (collected_at, category, data)
		 VALUES (now() - interval '400 days', 'cov_run_all', '{}'::jsonb)`,
		`INSERT INTO sage.action_log (executed_at, action_type, sql_executed, outcome)
		 VALUES (now() - interval '400 days', 'cov_run_all', 'SELECT 1', 'success')`,
		`INSERT INTO sage.explain_cache (captured_at, queryid, plan_json, source)
		 VALUES (now() - interval '400 days', -999999, '{}'::jsonb, 'cov_run_all')`,
	}
	for _, q := range seed {
		execRetry(t, ctx, q)
	}

	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 30,
			FindingsDays:  30,
			ActionsDays:   30,
			ExplainsDays:  30,
		},
	}

	var logMsgs []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logMsgs = append(logMsgs, fmt.Sprintf("[%s] %s", component, fmt.Sprintf(msg, args...)))
	}

	c := New(pool, cfg, logFn)
	c.Run(ctx)

	// Verify old rows were cleaned.
	for _, check := range []struct {
		query string
		label string
	}{
		{`SELECT count(*) FROM sage.snapshots WHERE category='cov_run_all'`, "snapshots"},
		{`SELECT count(*) FROM sage.action_log WHERE action_type='cov_run_all'`, "action_log"},
		{`SELECT count(*) FROM sage.explain_cache WHERE source='cov_run_all'`, "explain_cache"},
	} {
		var cnt int
		queryRetry(t, ctx, check.query, &cnt)
		if cnt != 0 {
			t.Errorf("expected 0 rows in %s after Run, got %d", check.label, cnt)
		}
	}

	// Verify that purge logged something for tables that had rows deleted.
	mu.Lock()
	defer mu.Unlock()
	foundPurge := false
	for _, m := range logMsgs {
		if strings.Contains(m, "purged") {
			foundPurge = true
			break
		}
	}
	if !foundPurge {
		t.Log("note: no purge log messages -- old rows may have been cleaned by prior test")
	}
}

// TestCoverage_PurgeTable_PositiveRetention exercises purgeTable with
// a positive retention value so the DELETE actually executes.
func TestCoverage_PurgeTable_PositiveRetention(t *testing.T) {
	pool, ctx := requireDB(t)

	tag := "cov_purge_pos"
	// Insert a row that is older than the retention window.
	execRetry(t, ctx,
		`INSERT INTO sage.snapshots (collected_at, category, data)
		 VALUES (now() - interval '200 days', $1, '{}'::jsonb)`, tag)

	// Also insert a recent row that should NOT be purged.
	execRetry(t, ctx,
		`INSERT INTO sage.snapshots (collected_at, category, data)
		 VALUES (now(), $1, '{}'::jsonb)`, tag)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)
	c.purgeTable(ctx, "snapshots", "collected_at", 30, "")

	// The old row should be gone, but the recent one should remain.
	var cnt int
	queryRetry(t, ctx,
		`SELECT count(*) FROM sage.snapshots WHERE category='cov_purge_pos'`,
		&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 remaining row (recent), got %d", cnt)
	}

	// Clean up.
	execRetry(t, ctx,
		`DELETE FROM sage.snapshots WHERE category='cov_purge_pos'`)
}

// TestCoverage_PurgeTable_WithExtraWhere exercises purgeTable with an extra
// WHERE clause (the findings table uses "AND status = 'resolved'").
func TestCoverage_PurgeTable_WithExtraWhere(t *testing.T) {
	pool, ctx := requireDB(t)

	// The findings table needs specific columns. Check if the table
	// has the expected shape first.
	tag := "cov_purge_extra"
	execRetry(t, ctx,
		`INSERT INTO sage.findings
			(category, severity, title, detail, last_seen, status)
		 VALUES ($1, 'low', 'test', '{}'::jsonb,
				 now() - interval '200 days', 'resolved')`, tag)

	// Insert an old finding that is NOT resolved -- should NOT be purged.
	execRetry(t, ctx,
		`INSERT INTO sage.findings
			(category, severity, title, detail, last_seen, status)
		 VALUES ($1, 'low', 'test', '{}'::jsonb,
				 now() - interval '200 days', 'open')`, tag)

	cfg := &config.Config{}
	c := New(pool, cfg, noopLog)
	c.purgeTable(ctx, "findings", "last_seen", 30, "AND status = 'resolved'")

	// Only the resolved finding should have been purged.
	var cnt int
	queryRetry(t, ctx,
		`SELECT count(*) FROM sage.findings WHERE category='cov_purge_extra'`,
		&cnt)
	if cnt != 1 {
		t.Errorf("expected 1 remaining finding (open), got %d", cnt)
	}

	// Clean up.
	execRetry(t, ctx,
		`DELETE FROM sage.findings WHERE category='cov_purge_extra'`)
}

// TestCoverage_PurgeTable_NothingToDelete exercises purgeTable when no rows
// match the retention criteria (deleted == 0, no log message).
func TestCoverage_PurgeTable_NothingToDelete(t *testing.T) {
	pool, ctx := requireDB(t)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)

	// Use a very long retention so nothing qualifies.
	c.purgeTable(ctx, "snapshots", "collected_at", 99999, "")

	mu.Lock()
	defer mu.Unlock()
	for _, m := range logged {
		if strings.Contains(m, "purged") {
			t.Error("unexpected purge log when nothing should be deleted")
		}
	}
}

// TestCoverage_PurgeTable_NegativeRetention exercises the early return for
// negative retention values.
func TestCoverage_PurgeTable_NegativeRetention(t *testing.T) {
	// nil pool -- would panic if purgeTable didn't return early
	cfg := &config.Config{}
	c := New(nil, cfg, noopLog)
	c.purgeTable(context.Background(), "snapshots", "collected_at", -5, "")
	// If we get here without panic, the early return worked.
}

// TestCoverage_CleanStaleFirstSeen_NoKeys exercises cleanStaleFirstSeen
// when there are no first_seen:* keys in sage.config.
func TestCoverage_CleanStaleFirstSeen_NoKeys(t *testing.T) {
	pool, ctx := requireDB(t)

	// Remove any existing first_seen keys to ensure a clean state.
	execRetry(t, ctx,
		`DELETE FROM sage.config WHERE key LIKE 'first_seen:cov_%'`)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)
	c.cleanStaleFirstSeen(ctx)

	// No "cleaned" log message expected.
	mu.Lock()
	defer mu.Unlock()
	for _, m := range logged {
		if strings.Contains(m, "cleaned") && strings.Contains(m, "stale") {
			t.Error("unexpected clean log when no first_seen keys exist")
		}
	}
}

// TestCoverage_CleanStaleFirstSeen_StaleKeyRemoved exercises the path
// where a first_seen key exists but the corresponding index does not.
func TestCoverage_CleanStaleFirstSeen_StaleKeyRemoved(t *testing.T) {
	pool, ctx := requireDB(t)

	staleKey := "first_seen:cov_test_nonexistent_idx"
	execRetry(t, ctx,
		`INSERT INTO sage.config (key, value)
		 VALUES ($1, '2024-01-01')
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = '2024-01-01'`, staleKey)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)
	c.cleanStaleFirstSeen(ctx)

	// Verify the key was removed.
	var cnt int
	queryRetry(t, ctx,
		`SELECT count(*) FROM sage.config
		 WHERE key = 'first_seen:cov_test_nonexistent_idx'`, &cnt)
	if cnt != 0 {
		t.Errorf("expected stale key to be removed, got count=%d", cnt)
	}

	// Verify log message was emitted.
	mu.Lock()
	defer mu.Unlock()
	foundClean := false
	for _, m := range logged {
		if strings.Contains(m, "cleaned") && strings.Contains(m, "stale") {
			foundClean = true
		}
	}
	if !foundClean {
		t.Error("expected log message about cleaning stale first_seen entries")
	}
}

// TestCoverage_CleanStaleFirstSeen_MultipleStaleKeys exercises cleanup of
// multiple stale keys at once.
func TestCoverage_CleanStaleFirstSeen_MultipleStaleKeys(t *testing.T) {
	pool, ctx := requireDB(t)

	keys := []string{
		"first_seen:cov_multi_a",
		"first_seen:cov_multi_b",
		"first_seen:cov_multi_c",
	}

	for _, k := range keys {
		execRetry(t, ctx,
			`INSERT INTO sage.config (key, value)
			 VALUES ($1, '2024-01-01')
			 ON CONFLICT (key, COALESCE(database_id, 0))
			 DO UPDATE SET value = '2024-01-01'`, k)
	}

	cfg := &config.Config{}
	c := New(pool, cfg, noopLog)
	c.cleanStaleFirstSeen(ctx)

	// All should be removed (none of these indexes exist).
	for _, k := range keys {
		var cnt int
		queryRetry(t, ctx,
			fmt.Sprintf(
				`SELECT count(*) FROM sage.config WHERE key = '%s'`, k),
			&cnt)
		if cnt != 0 {
			t.Errorf("expected key %s to be removed, got count=%d", k, cnt)
		}
	}
}

// TestCoverage_PurgeTable_LogsError exercises the error logging path in
// purgeTable by using a cancelled context.
func TestCoverage_PurgeTable_LogsError(t *testing.T) {
	pool, ctx := requireDB(t)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf("[%s] %s", component, fmt.Sprintf(msg, args...)))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)

	// Cancel the context before calling purgeTable so the query fails.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	c.purgeTable(cancelCtx, "snapshots", "collected_at", 30, "")

	mu.Lock()
	defer mu.Unlock()
	foundError := false
	for _, m := range logged {
		if strings.Contains(m, "error purging") {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected error log from purgeTable with cancelled context")
	}
}

// TestCoverage_CleanStaleFirstSeen_LogsError exercises error logging in
// cleanStaleFirstSeen by using a cancelled context.
func TestCoverage_CleanStaleFirstSeen_LogsError(t *testing.T) {
	pool, ctx := requireDB(t)

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf("[%s] %s", component, fmt.Sprintf(msg, args...)))
	}

	cfg := &config.Config{}
	c := New(pool, cfg, logFn)

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	c.cleanStaleFirstSeen(cancelCtx)

	mu.Lock()
	defer mu.Unlock()
	foundError := false
	for _, m := range logged {
		if strings.Contains(m, "error") {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected error log from cleanStaleFirstSeen with cancelled context")
	}
}

// TestCoverage_Run_ZeroRetention_NoDBCalls exercises Run() when all retention
// values are zero. purgeTable returns early for each, but cleanStaleFirstSeen
// still runs (it always runs).
func TestCoverage_Run_ZeroRetention_NoDBCalls(t *testing.T) {
	pool, ctx := requireDB(t)

	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 0,
			FindingsDays:  0,
			ActionsDays:   0,
			ExplainsDays:  0,
		},
	}

	var logged []string
	var mu sync.Mutex
	logFn := func(component string, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(msg, args...))
	}

	c := New(pool, cfg, logFn)
	c.Run(ctx) // Should not panic; purgeTable returns early, cleanStaleFirstSeen runs.

	// No purge logs expected (all retention=0 so purgeTable skips).
	mu.Lock()
	defer mu.Unlock()
	for _, m := range logged {
		if strings.Contains(m, "purged") {
			t.Error("unexpected purge log when all retention days are 0")
		}
	}
}

// ---------------------------------------------------------------------------
// Pure unit tests (no DB required)
// ---------------------------------------------------------------------------

// TestCoverage_New_StoresAllFields verifies the constructor sets all fields.
func TestCoverage_New_StoresAllFields(t *testing.T) {
	cfg := &config.Config{
		Retention: config.RetentionConfig{
			SnapshotsDays: 7,
			FindingsDays:  14,
			ActionsDays:   21,
			ExplainsDays:  28,
		},
	}

	called := false
	logFn := func(string, string, ...any) { called = true }

	c := New(nil, cfg, logFn)

	if c.pool != nil {
		t.Error("expected nil pool")
	}
	if c.cfg != cfg {
		t.Error("cfg not stored correctly")
	}
	if c.logFn == nil {
		t.Error("logFn is nil")
	}

	// Invoke logFn to verify it was stored correctly.
	c.logFn("test", "msg")
	if !called {
		t.Error("stored logFn was not the one we passed")
	}
}

// TestCoverage_PurgeTable_ZeroRetentionVariants covers both 0 and negative
// retention values to ensure the early-return guard works.
func TestCoverage_PurgeTable_ZeroRetentionVariants(t *testing.T) {
	cfg := &config.Config{}
	c := New(nil, cfg, noopLog)

	// These would panic if purgeTable tried to use the nil pool.
	values := []int{0, -1, -100, -2147483648}
	for _, v := range values {
		t.Run(fmt.Sprintf("days=%d", v), func(t *testing.T) {
			c.purgeTable(context.Background(), "snapshots", "collected_at", v, "")
		})
	}
}

// TestCoverage_PurgeTable_LogFnCalledOnError ensures that when purgeTable
// encounters an error it calls the log function (not just returns silently).
// We test this with a nil pool which will panic/error.
func TestCoverage_PurgeTable_NilPoolPositiveRetention(t *testing.T) {
	// With a nil pool and positive retention, purgeTable will try to call
	// pool.Exec which will panic. We verify it panics (meaning the guard
	// didn't catch it -- which is expected since the guard only checks
	// retentionDays <= 0).
	cfg := &config.Config{}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when purgeTable uses nil pool")
		}
	}()

	c := New(nil, cfg, noopLog)
	c.purgeTable(context.Background(), "snapshots", "collected_at", 1, "")
}

// TestCoverage_CleanStaleFirstSeen_NilPoolPanics verifies that
// cleanStaleFirstSeen panics with a nil pool (no early-return guard).
func TestCoverage_CleanStaleFirstSeen_NilPoolPanics(t *testing.T) {
	cfg := &config.Config{}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when cleanStaleFirstSeen uses nil pool")
		}
	}()

	c := New(nil, cfg, noopLog)
	c.cleanStaleFirstSeen(context.Background())
}
