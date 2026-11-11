package optimizer

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// noopLog2 is a no-op logger for phase 2 tests.
// (noopLog is already defined in validate_test.go.)
func noopLog2(_ string, _ string, _ ...any) {}

// ---------------------------------------------------------------------------
// installedExtensions (was extensionInstalled, 40%)
// ---------------------------------------------------------------------------

func TestPhase2_ExtensionInstalled_NilPool(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	// With nil pool the extension check cannot read and admits
	// (PostgreSQL validates the operator class).
	ok, reason := v.checkExtensionRequired(context.Background(), Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY i ON t USING gin (c gin_trgm_ops)"}, TableContext{})
	if !ok {
		t.Errorf("nil pool should admit (cannot check), got %q", reason)
	}
}

func TestPhase2_ExtensionInstalled_WithDB_PgTrgm(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	want := catalogHasExtension(t, pool, "pg_trgm")
	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	if got := installedForTest(t, v, context.Background(), "pg_trgm"); got != want {
		t.Fatalf("extensionInstalled(pg_trgm) = %v, pg_extension says %v", got, want)
	}
	if installedForTest(t, v, context.Background(), "pg_sage_no_such_extension") {
		t.Fatal("an extension missing from pg_extension reported installed")
	}
}

// catalogHasExtension reads pg_extension directly, independent of the
// validator under test.
func catalogHasExtension(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = $1)", name).
		Scan(&ok); err != nil {
		t.Fatalf("read pg_extension: %v", err)
	}
	return ok
}

func TestPhase2_ExtensionInstalled_WithDB_NonExistent(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	got := installedForTest(t, v,
		context.Background(), "nonexistent_ext_xyz",
	)
	if got {
		t.Error("nonexistent extension should return false")
	}
}

func TestPhase2_ExtensionInstalled_WithDB_Plpgsql(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	// plpgsql is always installed in PostgreSQL.
	got := installedForTest(t, v, context.Background(), "plpgsql")
	if !got {
		t.Error("plpgsql should always be installed")
	}
}

func TestPhase2_ExtensionInstalled_EmptyName(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	got := installedForTest(t, v, context.Background(), "")
	if got {
		t.Error("empty extension name should return false")
	}
}

// ---------------------------------------------------------------------------
// checkExtensionRequired — covers the gin+pg_trgm and gist+postgis paths
// ---------------------------------------------------------------------------

func TestPhase2_CheckExtensionRequired_GINTrgmNilPool(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t USING gin (col gin_trgm_ops)",
		IndexType: "gin",
	}
	ok, reason := v.checkExtensionRequired(
		context.Background(), rec, TableContext{},
	)
	// nil pool -> the check cannot read -> accepted
	if !ok {
		t.Errorf("should accept with nil pool, got rejected: %s",
			reason)
	}
}

func TestPhase2_CheckExtensionRequired_GISTPostgisNilPool(
	t *testing.T,
) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t USING gist (geom geometry)",
		IndexType: "gist",
	}
	ok, reason := v.checkExtensionRequired(
		context.Background(), rec, TableContext{},
	)
	if !ok {
		t.Errorf("should accept with nil pool, got rejected: %s",
			reason)
	}
}

func TestPhase2_CheckExtensionRequired_BtreeNoCheck(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (col)",
		IndexType: "btree",
	}
	ok, _ := v.checkExtensionRequired(context.Background(), rec, TableContext{})
	if !ok {
		t.Error("btree should not require extension check")
	}
}

func TestPhase2_CheckExtensionRequired_GINTrgm_WithDB(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	installed := catalogHasExtension(t, pool, "pg_trgm")
	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t USING gin (col gin_trgm_ops)",
		IndexType: "gin",
	}
	ok, reason := v.checkExtensionRequired(context.Background(), rec, TableContext{})
	if ok != installed {
		t.Fatalf("trigram GIN admitted = %v (%q), pg_trgm installed = %v", ok, reason, installed)
	}
	if !ok && !strings.Contains(reason, "pg_trgm") {
		t.Fatalf("rejection reason %q does not name pg_trgm", reason)
	}
}

// ---------------------------------------------------------------------------
// checkExpressionVolatility (was 60%)
// ---------------------------------------------------------------------------

func TestPhase2_CheckExpressionVolatility_NoExpression(
	t *testing.T,
) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (col1, col2)",
	}
	ok, _ := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	if !ok {
		t.Error("simple column index should pass volatility check")
	}
}

func TestPhase2_CheckExpressionVolatility_NoParen(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{DDL: "no parens at all"}
	ok, _ := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	if !ok {
		t.Error("DDL without parentheses should pass")
	}
}

func TestPhase2_CheckExpressionVolatility_NilPool(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (lower(col))",
	}
	ok, _ := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	// nil pool -> can't check -> allow
	if !ok {
		t.Error("nil pool should allow expression index")
	}
}

func TestPhase2_CheckExpressionVolatility_ImmutableFunc(
	t *testing.T,
) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	// lower() is IMMUTABLE in PostgreSQL.
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (lower(name))",
	}
	ok, reason := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	if !ok {
		t.Errorf("lower() is immutable, should pass: %s", reason)
	}
}

func TestPhase2_CheckExpressionVolatility_VolatileFunc(
	t *testing.T,
) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	// random() is VOLATILE in PostgreSQL.
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (random())",
	}
	ok, reason := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	if ok {
		t.Error("random() is volatile, should be rejected")
	}
	if reason == "" {
		t.Error("rejection should include a reason")
	}
}

func TestPhase2_CheckExpressionVolatility_StableFunc(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	// now() is STABLE in PostgreSQL.
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (now())",
	}
	ok, _ := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	if ok {
		t.Error("now() is stable (not immutable), should reject")
	}
}

func TestPhase2_CheckExpressionVolatility_UnknownFunc(
	t *testing.T,
) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY idx ON t (nonexistent_fn_xyz(col))",
	}
	ok, reason := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	// security-01b: a name with no function in pg_catalog or an
	// allowlisted extension is refused, not left to PostgreSQL.
	if ok || !strings.HasPrefix(reason, "function_schema_not_allowed") {
		t.Errorf("unknown function: ok=%t reason=%q, want function_schema_not_allowed",
			ok, reason)
	}
}

// ---------------------------------------------------------------------------
// maxNewPerTable (was 66.7%)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// enrichWithHypoPG (was 15.4%) — test without HypoPG available
// ---------------------------------------------------------------------------

func TestPhase2_EnrichWithHypoPG_NotAvailable(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	hypopg := NewHypoPG(pool, noopLog2)
	o := &Optimizer{
		hypopg: hypopg,
		whatIf: hypopg,
		logFn:  noopLog2,
	}

	rec := Recommendation{
		Table:     "test_table",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON test_table (id)",
		Severity:  "warning",
		IndexType: "btree",
	}
	tc := TableContext{
		Schema: "public",
		Table:  "test_table",
	}

	result, _ := o.enrichWithHypoPG(context.Background(), rec, tc)

	// HypoPG is almost certainly not installed in test DB.
	// The rec should be returned unchanged.
	if result.Validated {
		// Only fails if HypoPG IS installed — not an error per se.
		t.Log("HypoPG appears to be installed; skipping assertion")
	}
	if result.Table != "test_table" {
		t.Errorf("table should be preserved, got %q", result.Table)
	}
	if result.DDL != rec.DDL {
		t.Errorf("DDL should be preserved, got %q", result.DDL)
	}
}

func TestPhase2_EnrichWithHypoPG_CachedAvailability(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	hypopg := NewHypoPG(pool, noopLog2)
	// Call IsAvailable twice to exercise the cache path.
	a1 := hypopg.IsAvailable(context.Background())
	a2 := hypopg.IsAvailable(context.Background())
	if a1 != a2 {
		t.Error("cached availability should return same result")
	}
}

// ---------------------------------------------------------------------------
// scoreConfidence — various signal combinations
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// checkBRINCorrelation — boundary tests
// ---------------------------------------------------------------------------

func TestPhase2_CheckBRINCorrelation_NonBRIN(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{IndexType: "btree"}
	ok, _ := v.checkBRINCorrelation(rec, TableContext{})
	if !ok {
		t.Error("non-BRIN should always pass")
	}
}

func TestPhase2_CheckBRINCorrelation_HighCorrelation(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		IndexType: "brin",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (created_at)",
	}
	tc := TableContext{
		ColStats: []ColStat{
			{Column: "created_at", Correlation: 0.95},
		},
	}
	ok, _ := v.checkBRINCorrelation(rec, tc)
	if !ok {
		t.Error("high correlation (0.95) should pass BRIN check")
	}
}

func TestPhase2_CheckBRINCorrelation_LowCorrelation(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		IndexType: "brin",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (status)",
	}
	tc := TableContext{
		ColStats: []ColStat{
			{Column: "status", Correlation: 0.3},
		},
	}
	ok, reason := v.checkBRINCorrelation(rec, tc)
	if ok {
		t.Error("low correlation (0.3) should fail BRIN check")
	}
	if reason == "" {
		t.Error("rejection should include reason")
	}
}

func TestPhase2_CheckBRINCorrelation_ExactThreshold(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		IndexType: "brin",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (id)",
	}
	tc := TableContext{
		ColStats: []ColStat{
			{Column: "id", Correlation: 0.8},
		},
	}
	ok, _ := v.checkBRINCorrelation(rec, tc)
	if !ok {
		t.Error("correlation exactly 0.8 should pass BRIN check")
	}
}

func TestPhase2_CheckBRINCorrelation_NegativeCorrelation(
	t *testing.T,
) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		IndexType: "brin",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (seq_id)",
	}
	tc := TableContext{
		ColStats: []ColStat{
			{Column: "seq_id", Correlation: -0.95},
		},
	}
	ok, _ := v.checkBRINCorrelation(rec, tc)
	// abs(-0.95) = 0.95 >= 0.8
	if !ok {
		t.Error("negative high correlation should pass BRIN check")
	}
}

func TestPhase2_CheckBRINCorrelation_MissingColStats(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		IndexType: "brin",
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t (unknown_col)",
	}
	tc := TableContext{
		ColStats: []ColStat{
			{Column: "other_col", Correlation: 0.1},
		},
	}
	ok, _ := v.checkBRINCorrelation(rec, tc)
	// Column not found in ColStats -> allow
	if !ok {
		t.Error("missing col stats should allow BRIN")
	}
}

// ---------------------------------------------------------------------------
// checkWriteImpact — boundary tests
// ---------------------------------------------------------------------------

func TestPhase2_CheckWriteImpact_DefaultThreshold(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{
		WriteHeavyRatioPct: 50,
	}, noopLog2)
	rec := Recommendation{EstimatedImprovementPct: 10}
	tc := TableContext{WriteRate: 60.0}
	ok, _ := v.checkWriteImpact(rec, tc)
	// WriteRate 60 > WriteHeavyRatioPct 50, improvement 10 < threshold 15
	if ok {
		t.Error("write-heavy with low improvement should be rejected")
	}
}

func TestPhase2_CheckWriteImpact_HighImprovement(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{
		WriteHeavyRatioPct: 50,
	}, noopLog2)
	rec := Recommendation{EstimatedImprovementPct: 20}
	tc := TableContext{WriteRate: 60.0}
	ok, _ := v.checkWriteImpact(rec, tc)
	// improvement 20 >= default threshold 15 -> accept
	if !ok {
		t.Error("high improvement should pass write impact check")
	}
}

// ---------------------------------------------------------------------------
// HypoPG.IsAvailable — DB-dependent tests
// ---------------------------------------------------------------------------

func TestPhase2_HypoPG_IsAvailable_CachesResult(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	h := NewHypoPG(pool, noopLog2)
	r1 := h.IsAvailable(context.Background())
	r2 := h.IsAvailable(context.Background())
	if r1 != r2 {
		t.Error("IsAvailable should return cached result")
	}
	if h.available == nil {
		t.Error("available should be non-nil after first call")
	}
}

// ---------------------------------------------------------------------------
// extractTotalCost — additional cases
// ---------------------------------------------------------------------------

func TestPhase2_ExtractTotalCost_NestedPlan(t *testing.T) {
	json := `[{"Plan":{"Total Cost":123.45,"Plans":[{"Total Cost":10}]}}]`
	got := extractTotalCost([]byte(json))
	if got != 123.45 {
		t.Errorf("expected 123.45, got %f", got)
	}
}

func TestPhase2_ExtractTotalCost_NullBytes(t *testing.T) {
	got := extractTotalCost(nil)
	if got != 0 {
		t.Errorf("nil input should return 0, got %f", got)
	}
}

// ---------------------------------------------------------------------------
// Helper: connect to test DB or skip
// ---------------------------------------------------------------------------

// installedForTest reads whether one extension is installed through the
// validator's bounded catalog read.
func installedForTest(t *testing.T, v *Validator, ctx context.Context, name string) bool {
	t.Helper()
	installed, err := v.installedExtensions(ctx, []string{name})
	if err != nil {
		t.Fatalf("read extension %q: %v", name, err)
	}
	return installed[name]
}

func connectTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("DB unavailable: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("DB ping failed: %v", err)
	}
	return pool
}
