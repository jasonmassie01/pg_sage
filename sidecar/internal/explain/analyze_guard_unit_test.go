package explain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Phase 0 #2: timeout_ms <= 0 must mean the configured default, never an
// unlimited statement_timeout.
func TestTimeoutNonPositiveUsesDefault(t *testing.T) {
	want := time.Duration(config.DefaultExplainTimeoutMs) * time.Millisecond
	for _, ms := range []int{0, -1, -5000} {
		ex := New(nil, &config.ExplainConfig{TimeoutMs: ms}, noopLogFn)
		if got := ex.timeout(); got != want {
			t.Errorf("TimeoutMs=%d: timeout() = %v, want %v", ms, got, want)
		}
		if got := statementTimeoutSQL(ex.timeout()); got !=
			"SET LOCAL statement_timeout = '10000ms'" {
			t.Errorf("TimeoutMs=%d: statementTimeoutSQL = %q", ms, got)
		}
	}
	ex := New(nil, &config.ExplainConfig{TimeoutMs: 2500}, noopLogFn)
	if got := ex.timeout(); got != 2500*time.Millisecond {
		t.Errorf("TimeoutMs=2500: timeout() = %v", got)
	}
	if got := statementTimeoutSQL(ex.timeout()); got !=
		"SET LOCAL statement_timeout = '2500ms'" {
		t.Errorf("statementTimeoutSQL = %q", got)
	}
	// Sub-millisecond durations never round down to 0 (unlimited).
	if got := statementTimeoutSQL(300 * time.Microsecond); got !=
		"SET LOCAL statement_timeout = '1ms'" {
		t.Errorf("sub-millisecond statementTimeoutSQL = %q", got)
	}
}

// The cache key separates plan-only from ANALYZE results.
func TestCacheKeyIncludesPlanOnly(t *testing.T) {
	q := "SELECT * FROM orders"
	if cacheKey(q, nil, true) == cacheKey(q, nil, false) {
		t.Error("plan-only and ANALYZE share a cache key")
	}
	if cacheKey(q, nil, false) != cacheKey("SELECT  *  FROM orders", nil, false) {
		t.Error("whitespace-equivalent queries must share a key")
	}
	if cacheKey(q, []string{"1"}, false) == cacheKey(q, []string{"2"}, false) {
		t.Error("different params share a key")
	}
}

// An LLM failure fallback is cached briefly, a real answer for the TTL.
func TestCacheTTLMinutes(t *testing.T) {
	cases := []struct {
		ttl, want int
		degraded  bool
	}{
		{60, 60, false}, {60, llmFallbackTTLMinutes, true},
		{0, config.DefaultExplainCacheTTLMinutes, false},
		{0, llmFallbackTTLMinutes, true}, {-3, config.DefaultExplainCacheTTLMinutes, false},
		{1, 1, true}, {240, 240, false},
	}
	for _, c := range cases {
		ex := New(nil, &config.ExplainConfig{CacheTTLMinutes: c.ttl}, noopLogFn)
		if got := ex.cacheTTLMinutes(c.degraded); got != c.want {
			t.Errorf("ttl=%d degraded=%v: got %d, want %d", c.ttl, c.degraded, got, c.want)
		}
	}
	if llmFallbackTTLMinutes < 1 || llmFallbackTTLMinutes > 5 {
		t.Errorf("llmFallbackTTLMinutes = %d, want a short positive TTL", llmFallbackTTLMinutes)
	}
}

func TestStructuralRefusal(t *testing.T) {
	cases := []struct {
		q    sqlast.ReadQuery
		want string
	}{
		{sqlast.ReadQuery{LockingClause: true}, "FOR UPDATE/SHARE"},
		{sqlast.ReadQuery{ModifiesData: true}, "data-modifying"},
		{sqlast.ReadQuery{SelectInto: true}, "SELECT INTO"},
		{sqlast.ReadQuery{Functions: []sqlast.QualifiedName{{Name: "query_to_xml"}}},
			"query_to_xml"},
		{sqlast.ReadQuery{Functions: []sqlast.QualifiedName{
			{Schema: "pg_catalog", Name: "table_to_xml"}}}, "table_to_xml"},
		{sqlast.ReadQuery{Functions: []sqlast.QualifiedName{
			{Schema: "ext", Name: "dblink_exec"}}}, "dblink_exec"},
		{sqlast.ReadQuery{Functions: []sqlast.QualifiedName{{Name: "pg_input_is_valid"}}},
			"pg_input_is_valid"},
		{sqlast.ReadQuery{AttributeCalls: []sqlast.QualifiedName{{Name: "database_to_xml"}}},
			"database_to_xml"},
		{sqlast.ReadQuery{Functions: []sqlast.QualifiedName{{Name: "lower"}}}, ""},
		{sqlast.ReadQuery{}, ""},
	}
	for _, c := range cases {
		got := structuralRefusal(c.q)
		if c.want == "" && got != "" {
			t.Errorf("structuralRefusal(%+v) = %q, want none", c.q, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("structuralRefusal(%+v) = %q, want it to mention %q", c.q, got, c.want)
		}
	}
}

func TestRelationKindRefusal(t *testing.T) {
	rel := sqlast.QualifiedName{Schema: "public", Name: "t"}
	cases := []struct {
		kind string
		rls  bool
		want string
	}{
		{"r", false, ""}, {"p", false, ""}, {"m", false, ""}, {"S", false, ""},
		{"t", false, ""}, {"f", false, "foreign table"},
		{"r", true, "row-level security"}, {"p", true, "row-level security"},
		{"c", false, "unsupported relation kind"}, {"", false, "unsupported relation kind"},
	}
	for _, c := range cases {
		got := relationKindRefusal(rel, c.kind, c.rls)
		if c.want == "" && got != "" {
			t.Errorf("kind=%q rls=%v: %q, want none", c.kind, c.rls, got)
		}
		if c.want != "" && (!strings.Contains(got, c.want) || !strings.Contains(got, "public.t")) {
			t.Errorf("kind=%q rls=%v: %q, want %q naming public.t", c.kind, c.rls, got, c.want)
		}
	}
}

func TestExplainNote(t *testing.T) {
	if got := explainNote(false, false, ""); got != "" {
		t.Errorf("analyzed note = %q, want empty", got)
	}
	if got := explainNote(false, true, ""); !strings.Contains(got, "parameters") {
		t.Errorf("params note = %q", got)
	}
	if got := explainNote(true, false, ""); !strings.Contains(got, "plan_only") {
		t.Errorf("plan_only note = %q", got)
	}
	got := explainNote(false, false, "calls volatile function pg_sleep")
	if !strings.Contains(got, "without ANALYZE") || !strings.Contains(got, "pg_sleep") {
		t.Errorf("refused note = %q", got)
	}
}

// Without the parse tree (no cgo) nothing is provably safe: ANALYZE is
// refused and the plan-only result is returned with the reason.
func TestExplainRefusesAnalyzeWhenInspectionUnavailable(t *testing.T) {
	srv := newFakePGServer(t, `[{"Plan":{"Node Type":"Result","Total Cost":0.01}}]`, false)
	defer srv.close()
	pool := fakePGPool(t, srv.addr())
	defer pool.Close()
	ex := New(pool, &config.ExplainConfig{TimeoutMs: 5000}, noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{}, sqlast.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := ex.Explain(ctx, ExplainRequest{Query: "SELECT 1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if result.ActualTimeMs != nil {
		t.Error("ANALYZE ran although the query could not be inspected")
	}
	if !strings.Contains(result.AnalyzeRefused, "inspect") {
		t.Errorf("AnalyzeRefused = %q, want an inspection reason", result.AnalyzeRefused)
	}
	if !strings.Contains(result.Note, result.AnalyzeRefused) {
		t.Errorf("Note = %q does not carry the reason", result.Note)
	}
}

// A function that cannot be verified against the catalog is unsafe. The
// fake server cannot answer catalog queries, so verification fails closed.
func TestExplainRefusesAnalyzeForUnresolvedFunction(t *testing.T) {
	srv := newFakePGServer(t, `[{"Plan":{"Node Type":"Result","Total Cost":0.01}}]`, false)
	defer srv.close()
	pool := fakePGPool(t, srv.addr())
	defer pool.Close()
	ex := New(pool, &config.ExplainConfig{TimeoutMs: 5000}, noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{Functions: []sqlast.QualifiedName{{Name: "mystery"}}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := ex.Explain(ctx, ExplainRequest{Query: "SELECT mystery()"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if result.ActualTimeMs != nil || !strings.Contains(result.AnalyzeRefused, "mystery") {
		t.Errorf("ActualTimeMs=%v AnalyzeRefused=%q, want a refusal naming mystery",
			result.ActualTimeMs, result.AnalyzeRefused)
	}
}

// A query that calls nothing and reads nothing needs no catalog lookups
// and keeps ANALYZE.
func TestExplainAnalyzesWhenInspectionFindsNothing(t *testing.T) {
	srv := newFakePGServer(t, `[{"Plan":{"Node Type":"Result","Total Cost":0.01,`+
		`"Actual Total Time":0.002},"Planning Time":0.05}]`, false)
	defer srv.close()
	pool := fakePGPool(t, srv.addr())
	defer pool.Close()
	ex := New(pool, &config.ExplainConfig{TimeoutMs: 5000}, noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) { return sqlast.ReadQuery{}, nil }
	result, err := ex.Explain(context.Background(), ExplainRequest{Query: "SELECT 1"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if result.ActualTimeMs == nil || result.AnalyzeRefused != "" || result.Note != "" {
		t.Errorf("ActualTimeMs=%v AnalyzeRefused=%q Note=%q, want analyzed",
			result.ActualTimeMs, result.AnalyzeRefused, result.Note)
	}
}

func TestInspectParseErrorRefuses(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{}, errors.Join(sqlast.ErrRejected, errors.New("syntax"))
	}
	got := ex.analyzeRefusal(context.Background(), nil, "SELECT")
	if !strings.Contains(got, "inspect") {
		t.Errorf("analyzeRefusal = %q, want an inspection refusal", got)
	}
}
