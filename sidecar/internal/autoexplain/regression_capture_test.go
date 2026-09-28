package autoexplain

import (
	"github.com/pg-sage/sidecar/internal/testdb"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

func bootstrapAuditFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap designated fixture schema: %v", err)
	}
}

func newTestCollector(pool *pgxpool.Pool, avail *Availability) *Collector {
	return NewCollector(pool, CollectorConfig{
		CollectIntervalSeconds: 60, MaxPlansPerCycle: 5, LogMinDurationMs: 100,
	}, avail, func(string, string, ...any) {})
}

// G1-B09: a parameterized query must not be planned with every parameter
// bound to NULL (which constant-folds to a degenerate "One-Time Filter:
// false" plan). On PG16+ the generic plan is captured and labeled as such.
func TestCaptureOnDemand_ParameterizedUsesGenericPlan(t *testing.T) {
	pool := requireAuditFixturePool(t)
	// Pre-PG16 behavior is TestCaptureOnDemand_ParameterizedPrePG16NotStored.
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	bootstrapAuditFixture(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS b09_orders
		(id int PRIMARY KEY, total numeric)`); err != nil {
		t.Fatal(err)
	}
	const queryID = int64(999999909)
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid = $1", queryID)
	}
	cleanup()
	t.Cleanup(cleanup)

	c := newTestCollector(pool, &Availability{Method: "unavailable"})
	query := "SELECT total FROM b09_orders WHERE id = $1"
	if err := c.captureOnDemand(ctx, queryID, query); err != nil {
		t.Fatalf("capture: %v", err)
	}
	var source, plan string
	if err := pool.QueryRow(ctx, `SELECT source, plan_json::text FROM sage.explain_cache
		WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1`, queryID).Scan(&source, &plan); err != nil {
		t.Fatalf("read captured plan: %v", err)
	}
	if source != "generic_plan" {
		t.Errorf("source = %q, want generic_plan (never auto_explain for a NULL-bound plan)", source)
	}
	if strings.Contains(plan, "One-Time Filter") || !strings.Contains(plan, "$1") {
		t.Errorf("stored plan is NULL-bound, not generic: %s", plan)
	}
}

// G1-B09: before PG16 there is no GENERIC_PLAN; a NULL-bound plan must not
// be stored at all (it would be preferred over better evidence).
func TestCaptureOnDemand_ParameterizedPrePG16NotStored(t *testing.T) {
	pool := requireAuditFixturePool(t)
	bootstrapAuditFixture(t, pool)
	ctx := context.Background()
	const queryID = int64(999999910)
	cleanup := func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid = $1", queryID)
	}
	cleanup()
	t.Cleanup(cleanup)

	c := newTestCollector(pool, &Availability{Method: "unavailable"})
	c.serverVersionNum = 150000
	err := c.captureOnDemand(ctx, queryID, "SELECT $1::integer + 1")
	if err != nil {
		t.Fatalf("capture returned error for a skipped parameterized query: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.explain_cache
		WHERE queryid = $1`, queryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stored %d NULL-bound plans on pre-PG16, want 0", n)
	}
}

// G1-B15: auto_explain settings used for capture must be transaction-local
// and must not leak into the pooled session used by other sidecar work.
func TestCaptureOnDemand_DoesNotLeakAutoExplainSettings(t *testing.T) {
	dsn := requireAuditFixturePool(t).Config().ConnString()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bootstrapAuditFixture(t, pool)
	ctx := context.Background()
	const queryID = int64(999999911)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.explain_cache WHERE queryid = $1", queryID)
	})
	c := newTestCollector(pool, &Availability{
		SessionLoad: true, Available: true, Method: "session_load"})
	captureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.captureOnDemand(captureCtx, queryID, "SELECT 1"); err != nil {
		t.Fatalf("capture: %v", err)
	}
	var minDuration, analyze string
	if err := pool.QueryRow(ctx, `SELECT current_setting('auto_explain.log_min_duration'),
		current_setting('auto_explain.log_analyze')`).Scan(&minDuration, &analyze); err != nil {
		t.Fatalf("read session settings: %v", err)
	}
	if minDuration != "-1" || analyze != "off" {
		t.Fatalf("auto_explain leaked into pooled session: log_min_duration=%s log_analyze=%s",
			minDuration, analyze)
	}
}

// G1-B30: a non-positive interval must not panic the process.
func TestRun_NonPositiveIntervalDoesNotPanic(t *testing.T) {
	var logged []string
	c := NewCollector(nil, CollectorConfig{CollectIntervalSeconds: 0},
		&Availability{}, func(level, msg string, _ ...any) {
			logged = append(logged, level+":"+msg)
		})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Run panicked with interval 0: %v", r)
			}
		}()
		c.Run(ctx)
	}()
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "|"), "interval") {
		t.Errorf("invalid interval not reported; logs = %q", logged)
	}
}
