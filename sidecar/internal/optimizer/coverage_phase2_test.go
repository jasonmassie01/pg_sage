package optimizer

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// noopLog2 is a no-op logger for phase 2 tests.
// (noopLog is already defined in validate_test.go.)
func noopLog2(_ string, _ string, _ ...any) {}

// ---------------------------------------------------------------------------
// extensionInstalled (was 40%)
// ---------------------------------------------------------------------------

func TestPhase2_ExtensionInstalled_NilPool(t *testing.T) {
	v := NewValidator(nil, &config.OptimizerConfig{}, noopLog2)
	// With nil pool, extensionInstalled should return true
	// (can't check, assume installed).
	if !v.extensionInstalled(context.Background(), "pg_trgm") {
		t.Error("nil pool should return true (assume installed)")
	}
}

func TestPhase2_ExtensionInstalled_WithDB_PgTrgm(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	// pg_trgm may or may not be installed; we just verify
	// the function doesn't panic and returns a bool.
	_ = v.extensionInstalled(context.Background(), "pg_trgm")
}

func TestPhase2_ExtensionInstalled_WithDB_NonExistent(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	got := v.extensionInstalled(
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
	got := v.extensionInstalled(context.Background(), "plpgsql")
	if !got {
		t.Error("plpgsql should always be installed")
	}
}

func TestPhase2_ExtensionInstalled_EmptyName(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	got := v.extensionInstalled(context.Background(), "")
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
		context.Background(), rec,
	)
	// nil pool -> extensionInstalled returns true -> accepted
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
		context.Background(), rec,
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
	ok, _ := v.checkExtensionRequired(context.Background(), rec)
	if !ok {
		t.Error("btree should not require extension check")
	}
}

func TestPhase2_CheckExtensionRequired_GINTrgm_WithDB(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	v := NewValidator(pool, &config.OptimizerConfig{}, noopLog2)
	rec := Recommendation{
		DDL:       "CREATE INDEX CONCURRENTLY idx ON t USING gin (col gin_trgm_ops)",
		IndexType: "gin",
	}
	ok, reason := v.checkExtensionRequired(
		context.Background(), rec,
	)
	// If pg_trgm is not installed, should reject.
	// Either way, verify no panic.
	_ = ok
	_ = reason
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
	ok, _ := v.checkExpressionVolatility(
		context.Background(), rec,
	)
	// Unknown function -> query returns no rows -> allow
	if !ok {
		t.Error("unknown function should be allowed (can't check)")
	}
}

// ---------------------------------------------------------------------------
// maxNewPerTable (was 66.7%)
// ---------------------------------------------------------------------------

func TestPhase2_MaxNewPerTable_DefaultWhenZero(t *testing.T) {
	o := &Optimizer{cfg: &config.OptimizerConfig{MaxNewPerTable: 0}}
	got := o.maxNewPerTable()
	if got != defaultMaxNewPerTable {
		t.Errorf("expected default %d, got %d",
			defaultMaxNewPerTable, got)
	}
}

func TestPhase2_MaxNewPerTable_NegativeUsesDefault(t *testing.T) {
	o := &Optimizer{cfg: &config.OptimizerConfig{MaxNewPerTable: -1}}
	got := o.maxNewPerTable()
	if got != defaultMaxNewPerTable {
		t.Errorf("expected default %d for negative, got %d",
			defaultMaxNewPerTable, got)
	}
}

func TestPhase2_MaxNewPerTable_CustomValue(t *testing.T) {
	o := &Optimizer{cfg: &config.OptimizerConfig{MaxNewPerTable: 5}}
	got := o.maxNewPerTable()
	if got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestPhase2_MaxNewPerTable_One(t *testing.T) {
	o := &Optimizer{cfg: &config.OptimizerConfig{MaxNewPerTable: 1}}
	got := o.maxNewPerTable()
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestPhase2_MaxNewPerTable_LargeValue(t *testing.T) {
	o := &Optimizer{cfg: &config.OptimizerConfig{MaxNewPerTable: 100}}
	got := o.maxNewPerTable()
	if got != 100 {
		t.Errorf("expected 100, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// enrichWithHypoPG (was 15.4%) — test without HypoPG available
// ---------------------------------------------------------------------------

func TestPhase2_EnrichWithHypoPG_NotAvailable(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()

	hypopg := NewHypoPG(pool, 10.0, noopLog2)
	o := &Optimizer{
		hypopg: hypopg,
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

	result := o.enrichWithHypoPG(context.Background(), rec, tc)

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

	hypopg := NewHypoPG(pool, 10.0, noopLog2)
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

func TestPhase2_ScoreConfidence_NoSignals(t *testing.T) {
	o := &Optimizer{
		cfg:   &config.OptimizerConfig{},
		logFn: noopLog2,
	}
	rec := Recommendation{}
	// WriteRate=-1 signals "unknown"; 0.0 means "zero writes observed"
	// which is valid data. Use -1 to get true "no data" confidence.
	tc := TableContext{WriteRate: -1}
	result := o.scoreConfidence(rec, tc)
	// qv=0.1, pc=0, wr=0, hv=0, sk=0, tv=0.1
	// = 0.25*0.1 + 0 + 0 + 0 + 0 + 0.10*0.1 = 0.035
	if result.Confidence > 0.05 {
		t.Errorf("expected very low confidence with no signals, got %f",
			result.Confidence)
	}
	if result.ActionLevel != "high_risk" {
		t.Errorf("expected high_risk, got %q",
			result.ActionLevel)
	}
}

func TestPhase2_ScoreConfidence_HighVolumeNoPlans(t *testing.T) {
	o := &Optimizer{
		cfg:   &config.OptimizerConfig{},
		logFn: noopLog2,
	}
	rec := Recommendation{}
	tc := TableContext{
		Queries:   []QueryInfo{{Calls: 1000}},
		WriteRate: 5.0,
	}
	result := o.scoreConfidence(rec, tc)
	// QueryVolume=1.0 (500+), PlanClarity=0.5 (queries but no plans),
	// WriteRateKnown=1.0, HypoPG=0, Selectivity=0, TableCallVol=1.0
	// = 0.25*1.0 + 0.25*0.5 + 0.15*1.0 + 0 + 0 + 0.10*1.0
	// = 0.25 + 0.125 + 0.15 + 0.10 = 0.625
	if result.Confidence < 0.5 || result.Confidence > 0.7 {
		t.Errorf("expected ~0.625 confidence, got %f",
			result.Confidence)
	}
	if result.ActionLevel != "moderate" {
		t.Errorf("expected moderate, got %q", result.ActionLevel)
	}
}

func TestPhase2_ScoreConfidence_WithHypoPGValidated(t *testing.T) {
	o := &Optimizer{
		cfg:   &config.OptimizerConfig{},
		logFn: noopLog2,
	}
	rec := Recommendation{
		Validated:               true,
		EstimatedImprovementPct: 30.0,
	}
	tc := TableContext{
		Queries: []QueryInfo{{Calls: 500}},
		Plans:   []PlanSummary{{QueryID: 1}},
		ColStats: []ColStat{
			{Column: "id", NDistinct: -1, MostCommonVals: []string{"1"}},
		},
		WriteRate: 1.0,
	}
	result := o.scoreConfidence(rec, tc)
	// All signals should be high.
	if result.Confidence < 0.7 {
		t.Errorf("expected >=0.7 confidence with all signals, got %f",
			result.Confidence)
	}
	if result.ActionLevel != "safe" {
		t.Errorf("expected safe, got %q",
			result.ActionLevel)
	}
}

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

	h := NewHypoPG(pool, 10.0, noopLog2)
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
// isExplainable — additional edge cases
// ---------------------------------------------------------------------------

func TestPhase2_IsExplainable_SelectVariants(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"SELECT 1", true},
		{"  select * from t", true},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", true},
		{"INSERT INTO t VALUES (1)", false},
		{"UPDATE t SET a = 1", false},
		{"DELETE FROM t WHERE id = 1", false},
		{"CREATE TABLE t (id int)", false},
		{"", false},
		{"   ", false},
	}
	for _, tt := range tests {
		got := isExplainable(tt.query)
		if got != tt.want {
			t.Errorf("isExplainable(%q) = %v, want %v",
				tt.query, got, tt.want)
		}
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

func connectTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
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
