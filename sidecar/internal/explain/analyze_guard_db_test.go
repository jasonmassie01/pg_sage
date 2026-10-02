//go:build cgo

package explain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const guardFixtureSQL = `
DROP SCHEMA IF EXISTS eg_shadow CASCADE;
CREATE SCHEMA eg_shadow;
CREATE FUNCTION eg_shadow.lower(text) RETURNS text LANGUAGE sql VOLATILE AS 'SELECT $1';
DROP TABLE IF EXISTS public.eg_probe, public.eg_rls CASCADE;
DROP SEQUENCE IF EXISTS public.eg_seq;
DROP DOMAIN IF EXISTS public.eg_dom;
CREATE TABLE public.eg_probe (v int);
INSERT INTO public.eg_probe VALUES (42);
CREATE SEQUENCE public.eg_seq;
CREATE OR REPLACE FUNCTION public.eg_volatile() RETURNS int LANGUAGE sql VOLATILE AS 'SELECT 1';
CREATE OR REPLACE FUNCTION public.eg_stable() RETURNS int LANGUAGE sql STABLE AS 'SELECT 2';
CREATE OR REPLACE FUNCTION public.upper(text) RETURNS text LANGUAGE sql VOLATILE AS 'SELECT $1';
CREATE VIEW public.eg_kill_view AS SELECT pg_terminate_backend(pid) AS k
	FROM pg_stat_activity WHERE application_name = 'eg_victim';
CREATE VIEW public.eg_nested_view AS SELECT * FROM public.eg_kill_view;
CREATE VIEW public.eg_safe_view AS SELECT v FROM public.eg_probe;
CREATE TABLE public.eg_rls (v int);
ALTER TABLE public.eg_rls ENABLE ROW LEVEL SECURITY;
CREATE OR REPLACE FUNCTION public.eg_volop(int, int) RETURNS bool LANGUAGE sql VOLATILE
	AS 'SELECT $1 = $2';
CREATE OPERATOR public.=== (LEFTARG = int, RIGHTARG = int, FUNCTION = public.eg_volop);
CREATE DOMAIN public.eg_dom AS int CHECK (VALUE > 0);
CREATE OR REPLACE FUNCTION public.eg_attr(public.eg_probe) RETURNS int LANGUAGE sql VOLATILE
	AS 'SELECT 1';
CREATE OR REPLACE FUNCTION public.eg_sfunc(int, int) RETURNS int LANGUAGE sql VOLATILE
	AS 'SELECT $1 + $2';
CREATE AGGREGATE public.eg_agg(int) (SFUNC = public.eg_sfunc, STYPE = int, INITCOND = '0');
`

const guardCleanupSQL = `
DROP SCHEMA IF EXISTS eg_shadow CASCADE;
DROP VIEW IF EXISTS public.eg_nested_view, public.eg_kill_view, public.eg_safe_view;
DROP AGGREGATE IF EXISTS public.eg_agg(int);
DROP OPERATOR IF EXISTS public.=== (int, int);
DROP TABLE IF EXISTS public.eg_probe, public.eg_rls CASCADE;
DROP SEQUENCE IF EXISTS public.eg_seq;
DROP DOMAIN IF EXISTS public.eg_dom;
DROP FUNCTION IF EXISTS public.eg_volatile(), public.eg_stable(), public.upper(text),
	public.eg_volop(int, int), public.eg_sfunc(int, int);
`

// victim is a separate session that pg_terminate_backend would kill.
func openVictim(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	cfg := pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "eg_victim"
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect victim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func assertVictimAlive(t *testing.T, victim *pgx.Conn) {
	t.Helper()
	if err := victim.Ping(context.Background()); err != nil {
		t.Fatalf("victim session was terminated: %v", err)
	}
}

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

func explainFresh(t *testing.T, ex *Explainer, query string) *ExplainResult {
	t.Helper()
	_, _ = ex.pool.Exec(context.Background(), "DELETE FROM sage.explain_results")
	start := time.Now()
	result, err := ex.Explain(context.Background(), ExplainRequest{Query: query})
	if err != nil {
		t.Fatalf("Explain(%q): %v", query, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Explain(%q) took %v", query, time.Since(start))
	}
	return result
}

func assertRefused(t *testing.T, ex *Explainer, query, reason string) {
	t.Helper()
	result := explainFresh(t, ex, query)
	if result.ActualTimeMs != nil {
		t.Errorf("ANALYZE executed %q", query)
	}
	if !strings.Contains(result.AnalyzeRefused, reason) {
		t.Errorf("AnalyzeRefused = %q, want it to mention %q", result.AnalyzeRefused, reason)
	}
	if !strings.Contains(result.Note, "without ANALYZE") || len(result.PlanJSON) == 0 {
		t.Errorf("plan-only fallback missing: note=%q plan=%s", result.Note, result.PlanJSON)
	}
}

func assertAnalyzed(t *testing.T, ex *Explainer, query string) {
	t.Helper()
	result := explainFresh(t, ex, query)
	if result.ActualTimeMs == nil || result.AnalyzeRefused != "" {
		t.Errorf("%q not analyzed: refused=%q", query, result.AnalyzeRefused)
	}
}

func assertSideEffectsAbsent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var pwned, rows int
	var seqCalled bool
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_stat_activity WHERE application_name = 'eg_pwned'),
		(SELECT count(*) FROM public.eg_probe),
		(SELECT is_called FROM public.eg_seq)`).Scan(&pwned, &rows, &seqCalled)
	if err != nil {
		t.Fatalf("read side effects: %v", err)
	}
	if pwned != 0 || rows != 1 || seqCalled {
		t.Errorf("side effects: pwned sessions=%d probe rows=%d seq called=%v",
			pwned, rows, seqCalled)
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
