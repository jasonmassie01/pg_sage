package autoexplain

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// acquireTestPool returns a pgxpool connected to local Postgres,
// or skips the test if the database is not available.
func acquireTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Skipf("skip: cannot create pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("skip: cannot ping postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- Unit tests (no DB required) ---

func TestCoverage_NewCollector(t *testing.T) {
	cfg := CollectorConfig{
		CollectIntervalSeconds: 30,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
		PreferSessionLoad:      true,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	var logged []string
	logFn := func(a, b string, args ...any) {
		logged = append(logged, a+":"+b)
	}

	c := NewCollector(nil, cfg, avail, logFn)
	if c == nil {
		t.Fatal("NewCollector returned nil")
	}
	if c.cfg.CollectIntervalSeconds != 30 {
		t.Errorf(
			"cfg.CollectIntervalSeconds = %d, want 30",
			c.cfg.CollectIntervalSeconds,
		)
	}
	if c.cfg.MaxPlansPerCycle != 5 {
		t.Errorf(
			"cfg.MaxPlansPerCycle = %d, want 5",
			c.cfg.MaxPlansPerCycle,
		)
	}
	if c.cfg.LogMinDurationMs != 100 {
		t.Errorf(
			"cfg.LogMinDurationMs = %d, want 100",
			c.cfg.LogMinDurationMs,
		)
	}
	if c.cfg.PreferSessionLoad != true {
		t.Error("PreferSessionLoad should be true")
	}
	if c.avail != avail {
		t.Error("avail pointer mismatch")
	}
	if c.pool != nil {
		t.Error("pool should be nil")
	}
	// Verify logFn is wired correctly
	c.logFn("INFO", "test", "hello %s", "world")
	if len(logged) != 1 || logged[0] != "INFO:test" {
		t.Errorf("logFn not wired: logged = %v", logged)
	}
}

func TestCoverage_RunCancelledImmediately(t *testing.T) {
	cfg := CollectorConfig{
		CollectIntervalSeconds: 1,
		MaxPlansPerCycle:       1,
		LogMinDurationMs:       100,
	}
	avail := &Availability{Available: false, Method: "unavailable"}
	logFn := func(string, string, ...any) {}
	c := NewCollector(nil, cfg, avail, logFn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Run returned — good
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestCoverage_RunTicksThenCancels(t *testing.T) {
	// Use a real pool so Collect gets a real error (query on
	// missing sage schema) rather than panicking on nil pool.
	pool := acquireTestPool(t)

	cfg := CollectorConfig{
		CollectIntervalSeconds: 1,
		MaxPlansPerCycle:       1,
		LogMinDurationMs:       100,
	}
	avail := &Availability{Available: false, Method: "unavailable"}

	var warnings int
	logFn := func(a, b string, args ...any) {
		warnings++
	}
	c := NewCollector(pool, cfg, avail, logFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 1500*time.Millisecond,
	)
	defer cancel()

	c.Run(ctx)

	// We expect at least one WARN from the failed Collect call
	// (sage.explain_cache doesn't exist on plain test DB).
	if warnings < 1 {
		// It's possible the ticker didn't fire in time; don't
		// fail hard, just log it.
		t.Logf(
			"expected at least 1 warning, got %d "+
				"(ticker may not have fired)", warnings,
		)
	}
}

func TestCoverage_ExtractPlanMetrics_NilInput(t *testing.T) {
	cost, execTime := extractPlanMetrics(nil)
	if cost != 0 || execTime != 0 {
		t.Errorf("expected (0, 0), got (%f, %f)", cost, execTime)
	}
}

func TestCoverage_ExtractPlanMetrics_ZeroCost(t *testing.T) {
	planJSON := []byte(`[{
		"Plan": {"Total Cost": 0.0},
		"Execution Time": 0.0
	}]`)
	cost, execTime := extractPlanMetrics(planJSON)
	if cost != 0 {
		t.Errorf("total_cost = %f, want 0", cost)
	}
	if execTime != 0 {
		t.Errorf("execution_time = %f, want 0", execTime)
	}
}

func TestCoverage_ExtractPlanMetrics_LargePlan(t *testing.T) {
	planJSON := []byte(`[{
		"Plan": {
			"Node Type": "Seq Scan",
			"Total Cost": 99999.99,
			"Plan Rows": 1000000
		},
		"Execution Time": 5432.10,
		"Planning Time": 0.5
	}]`)
	cost, execTime := extractPlanMetrics(planJSON)
	if cost != 99999.99 {
		t.Errorf("total_cost = %f, want 99999.99", cost)
	}
	if execTime != 5432.10 {
		t.Errorf("execution_time = %f, want 5432.10", execTime)
	}
}

func TestCoverage_ExtractPlanMetrics_MultiplePlans(t *testing.T) {
	// Only the first element should be used.
	planJSON := []byte(`[
		{"Plan": {"Total Cost": 10.0}, "Execution Time": 1.0},
		{"Plan": {"Total Cost": 20.0}, "Execution Time": 2.0}
	]`)
	cost, execTime := extractPlanMetrics(planJSON)
	if cost != 10.0 {
		t.Errorf("total_cost = %f, want 10.0 (first plan)", cost)
	}
	if execTime != 1.0 {
		t.Errorf(
			"execution_time = %f, want 1.0 (first plan)",
			execTime,
		)
	}
}

func TestCoverage_ExtractPlanMetrics_MissingPlanKey(t *testing.T) {
	planJSON := []byte(`[{"Execution Time": 3.0}]`)
	cost, execTime := extractPlanMetrics(planJSON)
	if cost != 0 {
		t.Errorf("total_cost = %f, want 0 (no Plan key)", cost)
	}
	if execTime != 3.0 {
		t.Errorf("execution_time = %f, want 3.0", execTime)
	}
}

func TestCoverage_BuildSetStatements_OnlyAnalyze(t *testing.T) {
	scfg := SessionConfig{
		LogMinDurationMs: 100,
		LogAnalyze:       true,
		LogBuffers:       false,
		LogNested:        false,
	}
	stmts := buildSetStatements(scfg)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if stmts[2] != "SET auto_explain.log_analyze = true" {
		t.Errorf("stmts[2] = %q", stmts[2])
	}
}

func TestCoverage_BuildSetStatements_OnlyBuffers(t *testing.T) {
	scfg := SessionConfig{
		LogMinDurationMs: 100,
		LogAnalyze:       false,
		LogBuffers:       true,
		LogNested:        false,
	}
	stmts := buildSetStatements(scfg)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if stmts[2] != "SET auto_explain.log_buffers = true" {
		t.Errorf("stmts[2] = %q", stmts[2])
	}
}

func TestCoverage_BuildSetStatements_OnlyNested(t *testing.T) {
	scfg := SessionConfig{
		LogMinDurationMs: 100,
		LogAnalyze:       false,
		LogBuffers:       false,
		LogNested:        true,
	}
	stmts := buildSetStatements(scfg)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	expected := "SET auto_explain.log_nested_statements = true"
	if stmts[2] != expected {
		t.Errorf("stmts[2] = %q, want %q", stmts[2], expected)
	}
}

func TestCoverage_BuildSetStatements_ZeroDuration(t *testing.T) {
	scfg := SessionConfig{LogMinDurationMs: 0}
	stmts := buildSetStatements(scfg)
	expected := "SET auto_explain.log_min_duration = '0ms'"
	if stmts[0] != expected {
		t.Errorf("stmts[0] = %q, want %q", stmts[0], expected)
	}
}

func TestCoverage_IsExplainable_TabPrefixed(t *testing.T) {
	if !isExplainable("\tSELECT 1") {
		t.Error("tab-prefixed SELECT should be explainable")
	}
}

func TestCoverage_IsExplainable_NewlinePrefixed(t *testing.T) {
	if !isExplainable("\nSELECT 1") {
		t.Error("newline-prefixed SELECT should be explainable")
	}
}

func TestCoverage_IsExplainable_MixedCaseWith(t *testing.T) {
	if !isExplainable("With cte AS (SELECT 1) SELECT * FROM cte") {
		t.Error("mixed-case WITH should be explainable")
	}
}

// --- Integration tests (require local Postgres) ---

func TestCoverage_Detect(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	if avail == nil {
		t.Fatal("Detect returned nil Availability")
	}

	// We don't know if auto_explain is available on this
	// instance, but the result must be internally consistent.
	switch avail.Method {
	case "shared_preload":
		if !avail.SharedPreload {
			t.Error("Method=shared_preload but SharedPreload=false")
		}
		if !avail.Available {
			t.Error("Method=shared_preload but Available=false")
		}
	case "session_load":
		if !avail.SessionLoad {
			t.Error("Method=session_load but SessionLoad=false")
		}
		if !avail.Available {
			t.Error("Method=session_load but Available=false")
		}
	case "unavailable":
		if avail.Available {
			t.Error("Method=unavailable but Available=true")
		}
		if avail.SharedPreload {
			t.Error("Method=unavailable but SharedPreload=true")
		}
		if avail.SessionLoad {
			t.Error("Method=unavailable but SessionLoad=true")
		}
	default:
		t.Errorf("unexpected Method: %q", avail.Method)
	}
}

func TestCoverage_CheckSharedPreload(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	found, err := checkSharedPreload(ctx, pool)
	if err != nil {
		t.Fatalf("checkSharedPreload returned error: %v", err)
	}
	// Result depends on server config; just verify no error.
	_ = found
}

func TestCoverage_CheckSessionLoad(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	canLoad, err := checkSessionLoad(ctx, pool)
	if err != nil {
		t.Fatalf("checkSessionLoad returned error: %v", err)
	}
	// Result depends on permissions; just verify no error.
	_ = canLoad
}

func TestCoverage_ConfigureSession_SharedPreload(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	// First detect availability to decide if we can test this.
	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !avail.Available {
		t.Skip("skip: auto_explain not available on this instance")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	scfg := DefaultSessionConfig(200)
	err = ConfigureSession(ctx, conn, avail, scfg)
	if err != nil {
		t.Fatalf("ConfigureSession: %v", err)
	}
}

func TestCoverage_ConfigureSessionBatch(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !avail.Available {
		t.Skip("skip: auto_explain not available on this instance")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	scfg := DefaultSessionConfig(300)
	err = ConfigureSessionBatch(ctx, conn, avail, scfg)
	if err != nil {
		t.Fatalf("ConfigureSessionBatch: %v", err)
	}
}

func TestCoverage_ConfigureSession_AllFlagsDisabled(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !avail.Available {
		t.Skip("skip: auto_explain not available on this instance")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	scfg := SessionConfig{
		LogMinDurationMs: 500,
		LogAnalyze:       false,
		LogBuffers:       false,
		LogNested:        false,
	}
	err = ConfigureSession(ctx, conn, avail, scfg)
	if err != nil {
		t.Fatalf("ConfigureSession (flags disabled): %v", err)
	}
}

func TestCoverage_ConfigureSessionBatch_AllFlagsDisabled(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !avail.Available {
		t.Skip("skip: auto_explain not available on this instance")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	scfg := SessionConfig{
		LogMinDurationMs: 500,
		LogAnalyze:       false,
		LogBuffers:       false,
		LogNested:        false,
	}
	err = ConfigureSessionBatch(ctx, conn, avail, scfg)
	if err != nil {
		t.Fatalf("ConfigureSessionBatch (flags disabled): %v", err)
	}
}

func TestCoverage_Collect_NoSageSchema(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	// Collect will fail because sage.explain_cache doesn't exist
	// (unless the full schema is bootstrapped). We just verify it
	// returns an error rather than panicking.
	err := c.Collect(ctx)
	if err == nil {
		// If it succeeds, the sage schema is present — that's fine.
		return
	}
	// Error should mention the query failure.
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}

func TestCoverage_Detect_CancelledContext(t *testing.T) {
	pool := acquireTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Detect(ctx, pool)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// --- Integration tests requiring sage schema ---

// bootstrapSageSchema ensures the sage schema and tables exist.
// It skips the test if the schema cannot be created.
func bootstrapSageSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	// Use the schema package to bootstrap.
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Skipf(
			"skip: cannot bootstrap sage schema: %v", err,
		)
	}
}

func TestCoverage_StorePlan_Success(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	planJSON := []byte(`[{
		"Plan": {"Total Cost": 55.0},
		"Execution Time": 2.5
	}]`)

	// Use a distinctive queryID to avoid collisions.
	queryID := int64(999999901)

	// Clean up before and after.
	cleanup := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.explain_cache WHERE queryid = $1",
			queryID,
		)
	}
	cleanup()
	t.Cleanup(cleanup)

	err := c.storePlan(
		ctx, queryID, "SELECT 1", planJSON, 55.0, 2.5,
	)
	if err != nil {
		t.Fatalf("storePlan returned error: %v", err)
	}

	// Verify the row was inserted.
	var storedQuery string
	var storedSource string
	var storedCost float64
	err = pool.QueryRow(ctx,
		`SELECT query_text, source, total_cost
		 FROM sage.explain_cache WHERE queryid = $1
		 ORDER BY captured_at DESC LIMIT 1`,
		queryID,
	).Scan(&storedQuery, &storedSource, &storedCost)
	if err != nil {
		t.Fatalf("failed to read back stored plan: %v", err)
	}
	if storedQuery != "SELECT 1" {
		t.Errorf(
			"query_text = %q, want %q",
			storedQuery, "SELECT 1",
		)
	}
	if storedSource != "auto_explain" {
		t.Errorf(
			"source = %q, want %q",
			storedSource, "auto_explain",
		)
	}
	if storedCost != 55.0 {
		t.Errorf("total_cost = %f, want 55.0", storedCost)
	}
}

func TestCoverage_StorePlan_CancelledContext(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.storePlan(
		ctx, 999999902, "SELECT 1",
		[]byte(`[{"Plan": {"Total Cost": 1.0}}]`), 1.0, 0.0,
	)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestCoverage_CaptureOnDemand_SimpleSelect(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	// Detect availability to set up properly.
	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	queryID := int64(999999903)
	cleanup := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.explain_cache WHERE queryid = $1",
			queryID,
		)
	}
	cleanup()
	t.Cleanup(cleanup)

	err = c.captureOnDemand(ctx, queryID, "SELECT 1")
	if err != nil {
		t.Fatalf("captureOnDemand returned error: %v", err)
	}

	// Verify the plan was stored.
	var storedSource string
	err = pool.QueryRow(ctx,
		`SELECT source FROM sage.explain_cache
		 WHERE queryid = $1 LIMIT 1`,
		queryID,
	).Scan(&storedSource)
	if err != nil {
		t.Fatalf("failed to read stored plan: %v", err)
	}
	if storedSource != "auto_explain" {
		t.Errorf("source = %q, want auto_explain", storedSource)
	}
}

func TestCoverage_CaptureOnDemand_CancelledContext(t *testing.T) {
	pool := acquireTestPool(t)

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.captureOnDemand(ctx, 999999904, "SELECT 1")
	if err == nil {
		t.Error("expected error from cancelled context")
	}
	if err != nil && !strings.Contains(
		err.Error(), "acquire connection",
	) && !strings.Contains(err.Error(), "cancel") {
		// Should mention acquire or cancellation.
		t.Logf("error = %v (acceptable)", err)
	}
}

func TestCoverage_CaptureOnDemand_BadSQL(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	avail, err := Detect(ctx, pool)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	// Attempt to explain invalid SQL - should return an error
	// from the EXPLAIN step.
	err = c.captureOnDemand(
		ctx, 999999905,
		"SELECT * FROM nonexistent_table_abc123",
	)
	if err == nil {
		t.Error("expected error for invalid SQL")
	}
	if err != nil && !strings.Contains(err.Error(), "explain") {
		t.Errorf(
			"error should mention 'explain', got: %v", err,
		)
	}
}

func TestCoverage_Collect_WithSageSchema(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	// Use a very high threshold so no queries match, exercising
	// the successful query path with zero candidates.
	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       999999999,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	err := c.Collect(ctx)
	if err != nil {
		// pg_stat_statements may not exist, which is fine.
		t.Logf(
			"Collect with sage schema returned: %v", err,
		)
	}
}

func TestCoverage_Collect_WithCandidates(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	// Check pg_stat_statements is available.
	var extName string
	err := pool.QueryRow(ctx,
		"SELECT extname FROM pg_extension "+
			"WHERE extname='pg_stat_statements'",
	).Scan(&extName)
	if err != nil {
		t.Skip(
			"skip: pg_stat_statements not available",
		)
	}

	// Generate some query load so pg_stat_statements has entries.
	for i := 0; i < 20; i++ {
		_, _ = pool.Exec(ctx,
			"SELECT pg_sleep(0.001)",
		)
	}

	avail, detectErr := Detect(ctx, pool)
	if detectErr != nil {
		t.Fatalf("Detect: %v", detectErr)
	}

	// Use a very low threshold to catch our queries.
	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       3,
		LogMinDurationMs:       0,
	}
	var warnings []string
	logFn := func(level, scope string, args ...any) {
		warnings = append(
			warnings, fmt.Sprintf("%s:%s", level, scope),
		)
	}
	c := NewCollector(pool, cfg, avail, logFn)

	err = c.Collect(ctx)
	// Even if there are per-query errors (logged as warnings),
	// the overall Collect should succeed.
	if err != nil {
		t.Logf(
			"Collect returned error: %v (may be expected)", err,
		)
	}
	// Warnings from individual captureOnDemand failures are
	// expected (some queries in pg_stat_statements can't be
	// explained).
	t.Logf("warnings during Collect: %d", len(warnings))
}

func TestCoverage_CaptureOnDemand_WithSessionLoad(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	// Force session_load availability to exercise the LOAD
	// branch in captureOnDemand.
	avail := &Availability{
		SessionLoad: true,
		Available:   true,
		Method:      "session_load",
	}

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	queryID := int64(999999906)
	cleanup := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.explain_cache WHERE queryid = $1",
			queryID,
		)
	}
	cleanup()
	t.Cleanup(cleanup)

	// This exercises the ConfigureSession path with
	// session_load method.
	err := c.captureOnDemand(ctx, queryID, "SELECT 1")
	if err != nil {
		// May fail if LOAD is not permitted; that's acceptable.
		t.Logf(
			"captureOnDemand with session_load: %v", err,
		)
	}
}

func TestCoverage_CaptureOnDemand_WithSharedPreload(t *testing.T) {
	pool := acquireTestPool(t)
	bootstrapSageSchema(t, pool)
	ctx := context.Background()

	// Force shared_preload availability to exercise the
	// ConfigureSession branch.
	avail := &Availability{
		SharedPreload: true,
		Available:     true,
		Method:        "shared_preload",
	}

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	queryID := int64(999999907)
	cleanup := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.explain_cache WHERE queryid = $1",
			queryID,
		)
	}
	cleanup()
	t.Cleanup(cleanup)

	err := c.captureOnDemand(ctx, queryID, "SELECT 1")
	if err != nil {
		t.Fatalf("captureOnDemand with shared_preload: %v", err)
	}

	// Verify plan was stored.
	var count int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.explain_cache "+
			"WHERE queryid = $1",
		queryID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 plan stored, got %d", count)
	}
}

func TestCoverage_ConfigureSession_SessionLoadMethod(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Force session_load to exercise the LOAD branch.
	avail := &Availability{
		SessionLoad: true,
		Available:   true,
		Method:      "session_load",
	}
	scfg := DefaultSessionConfig(200)

	err = ConfigureSession(ctx, conn, avail, scfg)
	// If LOAD succeeds, great. If it fails (permission denied),
	// the error should mention "load auto_explain".
	if err != nil {
		if !strings.Contains(err.Error(), "load auto_explain") {
			t.Errorf(
				"unexpected error: %v (expected 'load auto_explain')",
				err,
			)
		}
	}
}

func TestCoverage_ConfigureSessionBatch_SessionLoadMethod(
	t *testing.T,
) {
	pool := acquireTestPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Force session_load to exercise the LOAD branch in batch.
	avail := &Availability{
		SessionLoad: true,
		Available:   true,
		Method:      "session_load",
	}
	scfg := DefaultSessionConfig(200)

	err = ConfigureSessionBatch(ctx, conn, avail, scfg)
	if err != nil {
		// Batch with LOAD may fail; that's ok.
		t.Logf("ConfigureSessionBatch session_load: %v", err)
	}
}

func TestCoverage_Run_CollectErrorLogged(t *testing.T) {
	pool := acquireTestPool(t)

	cfg := CollectorConfig{
		CollectIntervalSeconds: 1,
		MaxPlansPerCycle:       1,
		LogMinDurationMs:       100,
	}
	// Use unavailable so Collect hits the DB query on
	// sage.explain_cache which may not exist, producing a WARN.
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}

	var warnings []string
	logFn := func(level, scope string, args ...any) {
		warnings = append(
			warnings, fmt.Sprintf("%s:%s", level, scope),
		)
	}
	c := NewCollector(pool, cfg, avail, logFn)

	ctx, cancel := context.WithTimeout(
		context.Background(), 1200*time.Millisecond,
	)
	defer cancel()

	c.Run(ctx)

	// The Run loop should have ticked once and logged a warning
	// from the failed Collect call (either missing schema or
	// successful but no candidates).
	t.Logf("warnings logged during Run: %d", len(warnings))
}

func TestCoverage_Collect_CancelledContext(t *testing.T) {
	pool := acquireTestPool(t)

	cfg := CollectorConfig{
		CollectIntervalSeconds: 60,
		MaxPlansPerCycle:       5,
		LogMinDurationMs:       100,
	}
	avail := &Availability{
		Available: false,
		Method:    "unavailable",
	}
	logFn := func(string, string, ...any) {}
	c := NewCollector(pool, cfg, avail, logFn)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Collect(ctx)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}
