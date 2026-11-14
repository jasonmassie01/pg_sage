//go:build cgo

package explain

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 0 #2: EXPLAIN ANALYZE executes the query, and a READ ONLY
// transaction does not stop volatile functions. Every case below requests
// ANALYZE; each must come back plan-only with a reason, without the side
// effect, while safe queries keep ANALYZE.
func TestExplainAnalyzeGuardRealPG(t *testing.T) {
	pool := liveExplainPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, guardFixtureSQL); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), guardCleanupSQL) })
	victim := openVictim(t, pool)
	refused := []struct{ query, reason string }{
		{"SELECT pg_terminate_backend(pid) FROM pg_stat_activity " +
			"WHERE application_name = 'eg_victim'", "pg_terminate_backend"},
		{"SELECT * FROM public.eg_kill_view", "pg_terminate_backend"},
		{"SELECT * FROM eg_nested_view", "pg_terminate_backend"},
		{"SELECT table_to_xml('public.eg_kill_view', true, false, '')", "table_to_xml"},
		{"SELECT set_config('application_name', 'eg_pwned', false)", "set_config"},
		{"SELECT nextval('public.eg_seq')", "nextval"},
		{"SELECT eg_volatile()", "eg_volatile"},
		{`SELECT public."eg_volatile"()`, "eg_volatile"},
		{"SELECT eg_shadow.lower('x')", "eg_shadow.lower"},
		{"SELECT upper('x')", "upper"},
		{"SELECT 1 === 2", "==="},
		{"SELECT 5::eg_dom", "eg_dom"},
		{"SELECT p.eg_attr FROM eg_probe p", "eg_attr"},
		{"SELECT eg_agg(v) FROM eg_probe", "eg_agg"},
		{"SELECT * FROM eg_rls", "row-level security"},
		{"SELECT * FROM eg_probe FOR UPDATE", "FOR UPDATE"},
		{"WITH d AS (DELETE FROM eg_probe RETURNING v) SELECT * FROM d", "data-modifying"},
	}
	for _, c := range refused {
		t.Run("refuse "+c.query, func(t *testing.T) {
			assertRefused(t, New(pool, liveConfig(5000), noopLogFn), c.query, c.reason)
			assertVictimAlive(t, victim)
		})
	}
	assertSideEffectsAbsent(t, pool)
	for _, q := range []string{
		"SELECT lower('X') FROM public.eg_probe",
		"SELECT eg_stable(), now(), count(*) FROM eg_probe WHERE v BETWEEN 1 AND 100",
		"SELECT * FROM eg_safe_view",
		"WITH s AS (SELECT v FROM eg_probe) SELECT v + 1 FROM s ORDER BY 1",
		"SELECT 5::int, '{1,2}'::int[], 'x'::text FROM eg_probe",
	} {
		t.Run("analyze "+q, func(t *testing.T) {
			assertAnalyzed(t, New(pool, liveConfig(5000), noopLogFn), q)
		})
	}
}

// Schema-aware resolution: putting a schema with a volatile shadow first on
// the search_path makes the same unqualified call unsafe.
func TestExplainAnalyzeGuardFollowsSearchPath(t *testing.T) {
	base := liveExplainPool(t)
	ctx := context.Background()
	if _, err := base.Exec(ctx, guardFixtureSQL); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = base.Exec(context.Background(), guardCleanupSQL) })
	assertAnalyzed(t, New(base, liveConfig(5000), noopLogFn), "SELECT lower('x')")
	cfg := base.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = "eg_shadow, public"
	cfg.MaxConns = 2
	shadowed, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("shadow pool: %v", err)
	}
	t.Cleanup(shadowed.Close)
	assertRefused(t, New(shadowed, liveConfig(5000), noopLogFn), "SELECT lower('x')",
		"lower")
}

func TestAnalyzeRefusalUnknownObjects(t *testing.T) {
	pool := liveExplainPool(t)
	ex := New(pool, liveConfig(5000), noopLogFn)
	for query, want := range map[string]string{
		"SELECT eg_missing_fn()":          "unknown function",
		"SELECT * FROM eg_missing_rel":    "unknown relation",
		"SELECT 1 OPERATOR(public.##?) 2": "unknown operator",
		"SELECT 1::eg_missing_type":       "unknown type",
	} {
		got := ex.analyzeRefusal(context.Background(), pool, query)
		if !strings.Contains(got, want) {
			t.Errorf("analyzeRefusal(%q) = %q, want %q", query, got, want)
		}
	}
	if got := ex.analyzeRefusal(context.Background(), pool,
		"WITH eg_missing_rel AS (SELECT 1) SELECT * FROM eg_missing_rel"); got != "" {
		t.Errorf("a CTE reference was treated as an unknown relation: %q", got)
	}
}

// env-cgo-c: the same live checks as the TestGuardCatalog* tests, with the
// real parser instead of recorded shapes. A failure here and a pass there
// means the parser gives a different shape than the one recorded in
// analyze_guard_shapes_test.go.

func TestGuardParserAnalyzesPlainSelect(t *testing.T) { checkGuardAnalyzesPlainSelect(t, nil) }

func TestGuardParserRefusesSideEffects(t *testing.T) { checkGuardRefusesSideEffects(t, nil) }

func TestGuardParserRefusesImplicitCast(t *testing.T) { checkGuardRefusesImplicitCast(t, nil) }

func TestGuardParserUnknownObjects(t *testing.T) { checkGuardUnknownObjects(t, nil) }

func TestGuardParserViewDepth(t *testing.T) { checkGuardViewDepth(t, nil) }

func TestGuardParserRereadsVolatility(t *testing.T) { checkGuardRereadsVolatility(t, nil) }

func TestGuardParserTimeoutBoundary(t *testing.T) { checkGuardTimeoutBoundary(t, nil) }

// Run under -race in CI: 16 guard calls share the Explainer and the parser.
func TestGuardParserConcurrent(t *testing.T) { checkGuardConcurrent(t, nil) }

func TestGuardParserRefusesDomainChecks(t *testing.T) { checkGuardRefusesDomainChecks(t, nil) }
