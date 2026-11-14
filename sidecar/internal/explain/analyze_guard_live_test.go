package explain

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Live analyze guard checks. Each check takes the parse-shape source: the
// TestGuardCatalog* tests below pass recorded shapes and run on every
// build; the cgo twins in analyze_guard_db_test.go pass nil (the parser).

func guardExplainer(pool *pgxpool.Pool, timeoutMs int, inspect inspectFn) *Explainer {
	ex := New(pool, liveConfig(timeoutMs), noopLogFn)
	if inspect != nil {
		ex.inspect = inspect
	}
	return ex
}

func checkGuardAnalyzesPlainSelect(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	result := explainFresh(t, guardExplainer(pool, 5000, inspect),
		"SELECT v FROM public.eg_probe")
	if result.ActualTimeMs == nil || result.AnalyzeRefused != "" {
		t.Fatalf("plain SELECT not analyzed: refused=%q", result.AnalyzeRefused)
	}
	if !strings.Contains(string(result.PlanJSON), `"Actual Rows": 1`) {
		t.Errorf("analyzed plan lacks the actual row count: %s", result.PlanJSON)
	}
}

func checkGuardRefusesSideEffects(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	execFixture(t, pool, hiddenFixtureSQL, hiddenCleanupSQL)
	victim := openVictim(t, pool)
	ex := guardExplainer(pool, 5000, inspect)
	for _, c := range []struct{ query, reason string }{
		{"SELECT nextval('public.eg_seq')", "volatile function nextval"},
		{"SELECT pg_advisory_lock(4242)", "volatile function pg_advisory_lock"},
		{"SELECT eg_volatile()", "volatile function eg_volatile"},
		{"SELECT 1 === 2", "operator === backed by a volatile function"},
		{"SELECT * FROM eg_probe FOR UPDATE", "FOR UPDATE/SHARE"},
		{"WITH d AS (DELETE FROM eg_probe RETURNING v) SELECT * FROM d", "data-modifying"},
		{"SELECT eg_defaulted()", "default argument"},
	} {
		t.Run(c.query, func(t *testing.T) {
			assertRefused(t, ex, c.query, c.reason)
			assertVictimAlive(t, victim)
			assertSideEffectsAbsent(t, pool)
			assertNoAdvisoryLocks(t, pool)
		})
	}
	assertAnalyzed(t, ex, "SELECT eg_const_default()")
}

// An implicit cast does not appear in the query text: length(x) on an enum
// column runs the cast function eg_enum_text, which takes a lock.
func checkGuardRefusesImplicitCast(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	execFixture(t, pool, castFixtureSQL, castCleanupSQL)
	ex := guardExplainer(pool, 5000, inspect)
	assertRefused(t, ex, "SELECT length(x) FROM public.eg_cast_t", "implicit cast")
	assertNoAdvisoryLocks(t, pool)
	assertRefused(t, ex, "SELECT v FROM public.eg_probe", "eg_enum_text")
	if _, err := pool.Exec(context.Background(), castCleanupSQL); err != nil {
		t.Fatalf("drop cast: %v", err)
	}
	assertAnalyzed(t, ex, "SELECT v FROM public.eg_probe")
}

func checkGuardUnknownObjects(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	ex := guardExplainer(pool, 5000, inspect)
	for query, want := range map[string]string{
		"SELECT eg_missing_fn()":          "calls unknown function eg_missing_fn",
		"SELECT * FROM eg_missing_rel":    "reads unknown relation eg_missing_rel",
		"SELECT 1 OPERATOR(public.##?) 2": "uses unknown operator public.##?",
		"SELECT 1::eg_missing_type":       "casts to unknown type eg_missing_type",
	} {
		got := ex.analyzeRefusal(context.Background(), pool, query)
		if got != want {
			t.Errorf("analyzeRefusal(%q) = %q, want %q", query, got, want)
		}
	}
}

// A view chain is followed four levels deep; the fifth level is refused.
func checkGuardViewDepth(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	execFixture(t, pool, hiddenFixtureSQL, hiddenCleanupSQL)
	ex := guardExplainer(pool, 5000, inspect)
	assertAnalyzed(t, ex, "SELECT * FROM eg_chain2")
	assertRefused(t, ex, "SELECT * FROM eg_chain1", "nested deeper than the guard follows")
}

// A function altered from IMMUTABLE to VOLATILE between two calls is
// refused on the second: the guard reads the catalog on every call.
func checkGuardRereadsVolatility(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL) // the chain views read eg_probe
	execFixture(t, pool, hiddenFixtureSQL, hiddenCleanupSQL)
	ex := guardExplainer(pool, 5000, inspect)
	const q = "SELECT eg_flip(1)"
	for i, state := range []string{"IMMUTABLE", "VOLATILE", "IMMUTABLE"} {
		_, err := pool.Exec(context.Background(), "ALTER FUNCTION public.eg_flip(int) "+state)
		if err != nil {
			t.Fatalf("alter eg_flip %s: %v", state, err)
		}
		if state == "VOLATILE" {
			assertRefused(t, ex, q, "volatile function eg_flip")
			continue
		}
		if got := ex.analyzeRefusal(context.Background(), pool, q); got != "" {
			t.Errorf("step %d (%s): refused %q", i, state, got)
		}
		assertAnalyzed(t, ex, q)
	}
}
