//go:build cgo

package sqlast

import (
	"strings"
	"testing"
)

// Owner decision 2026-10-04 (PR #110): CREATE STATISTICS and its inverse
// DROP STATISTICS are executor statements, limited by the parse tree to
// one pg_sage-named object (Rules.StatisticsName) on plain columns of one
// schema-qualified table, of the kinds the verifier understands.

func statsRules() Rules {
	rules := testRules
	rules.StatisticsName = func(name string) bool { return strings.HasPrefix(name, "sage_stx_") }
	return rules
}

func TestCheckAcceptsPgSageStatistics(t *testing.T) {
	for _, sql := range []string{
		"CREATE STATISTICS public.sage_stx_ab ON a, b FROM public.orders",
		"CREATE STATISTICS IF NOT EXISTS public.sage_stx_ab (ndistinct, dependencies, mcv) " +
			"ON a, b FROM public.orders",
		`CREATE STATISTICS "Sales".sage_stx_ab (mcv) ON "A", b FROM "Sales"."Orders"`,
		"DROP STATISTICS public.sage_stx_ab",
		"DROP STATISTICS IF EXISTS public.sage_stx_ab",
	} {
		if err := Check(sql, statsRules()); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestCheckRejectsOtherStatistics(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE STATISTICS public.st_ab ON a, b FROM public.orders":       "pg_sage",
		"CREATE STATISTICS sage_stx_ab ON a, b FROM public.orders":        "schema-qualified",
		"CREATE STATISTICS public.sage_stx_ab ON a, b FROM orders":        "schema-qualified",
		"CREATE STATISTICS app.sage_stx_ab ON a, b FROM public.orders":    "table's schema",
		"CREATE STATISTICS public.sage_stx_ab ON (a + 1), b FROM public.orders": "plain columns",
		"CREATE STATISTICS public.sage_stx_ab ON a, b FROM public.o, public.p":  "one table",
		"CREATE STATISTICS public.sage_stx_ab ON a, b FROM public.o JOIN public.p USING (a)": "one table",
		"CREATE STATISTICS sage.sage_stx_ab ON a, b FROM sage.findings":   "protected schema",
		"CREATE STATISTICS pg_catalog.sage_stx_ab ON a, b FROM pg_catalog.pg_class": "protected schema",
		"DROP STATISTICS public.st_ab":                     "pg_sage",
		"DROP STATISTICS sage_stx_ab":                      "schema-qualified",
		"DROP STATISTICS public.sage_stx_a, public.sage_stx_b": "one statistics object",
		"DROP STATISTICS public.sage_stx_ab CASCADE":       "CASCADE",
		"DROP STATISTICS sage.sage_stx_ab":                 "protected schema",
		"DROP TABLE public.orders":                         "only indexes",
	} {
		err := Check(sql, statsRules())
		if err == nil {
			t.Errorf("Check(%q) accepted, want refusal mentioning %q", sql, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want it to mention %q", sql, err, want)
		}
	}
}

// The parser sees kinds the text form cannot spell differently: an
// unknown kind is refused by structure as well.
func TestCheckRejectsUnknownStatisticsKind(t *testing.T) {
	err := Check("CREATE STATISTICS public.sage_stx_ab (bogus) ON a, b FROM public.orders",
		statsRules())
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("unknown kind = %v, want a kind refusal", err)
	}
}

// Without a StatisticsName rule no statistics object is pg_sage's own:
// both statements fail closed.
func TestCheckWithoutStatisticsRuleRefuses(t *testing.T) {
	for _, sql := range []string{
		"CREATE STATISTICS public.sage_stx_ab ON a, b FROM public.orders",
		"DROP STATISTICS public.sage_stx_ab",
	} {
		if err := Check(sql, testRules); err == nil {
			t.Errorf("Check(%q) accepted without a statistics name rule", sql)
		}
	}
}
