package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Dogfood round 2 (item 4): REINDEX and CREATE STATISTICS are verified
// classes. Before, the ledger could only carry their levels over: no
// verdict ever reached sage.action_outcome for them.

// No concurrent access tests: these are pure functions.

func TestVerificationClassStatisticsAndReindex(t *testing.T) {
	cases := map[string]string{
		"REINDEX INDEX CONCURRENTLY public.idx":                     verify.ClassReindex,
		"reindex table concurrently public.t":                       verify.ClassReindex,
		"REINDEX (VERBOSE) INDEX public.idx":                        verify.ClassReindex,
		"CREATE STATISTICS s_ab (dependencies) ON a, b FROM t":      verify.ClassStatistics,
		"create statistics if not exists public.s ON a, b FROM t":   verify.ClassStatistics,
		"CREATE STATISTICS public.s ON (lower(a)), b FROM public.t": verify.ClassStatistics,
	}
	for sql, want := range cases {
		if got := verificationClass(sql); got != want {
			t.Errorf("verificationClass(%q) = %q, want %q", sql, got, want)
		}
	}
}

// Both classes are watched by the post-action monitor even without a
// rollback statement: REINDEX has none, and an operator-approved CREATE
// STATISTICS may come without one. VACUUM/ANALYZE stay immediate.
func TestMonitoredWithoutRollback(t *testing.T) {
	cases := map[string]bool{
		"REINDEX INDEX CONCURRENTLY public.idx":         true,
		"CREATE STATISTICS s ON a, b FROM public.t":     true,
		"VACUUM public.t":                               false,
		"ANALYZE public.t":                              false,
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":   false,
		"SELECT pg_cancel_backend(42)":                  false,
		"":                                              false,
		"ALTER TABLE public.t SET (fillfactor = 90)":    false,
		"DELETE FROM public.events WHERE id < 10":       false,
		"DROP INDEX CONCURRENTLY public.idx":            false,
		"INSERT INTO hint_plan.hints VALUES (1, 'x')":   false,
		"ALTER SYSTEM SET work_mem = '64MB'":            false,
		"reindex (verbose) table concurrently public.t": true,
	}
	for sql, want := range cases {
		if got := monitoredWithoutRollback(sql); got != want {
			t.Errorf("monitoredWithoutRollback(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestPredictionForStatisticsAndReindex(t *testing.T) {
	p := predictionFromDetail(verify.ClassStatistics, map[string]any{"queryid": 42.0})
	if !p.Predicts() || p.Metric != verify.MetricRowEstimateError ||
		*p.ExpectedChangePct != -50 || p.Method != verify.MethodRule {
		t.Fatalf("statistics prediction = %+v, want a rule: row-estimate error -50%%", p)
	}
	if len(p.TargetQueryIDs) != 1 || p.TargetQueryIDs[0] != 42 {
		t.Fatalf("statistics targets = %v, want the finding's queryid", p.TargetQueryIDs)
	}
	r := predictionFromDetail(verify.ClassReindex, map[string]any{"bloat_pct": 35.0})
	if !r.Predicts() || r.Metric != verify.MetricIndexBytes || *r.ExpectedChangePct != -35 {
		t.Fatalf("reindex prediction = %+v, want index bytes -35%% (the measured bloat)", r)
	}
	r = predictionFromDetail(verify.ClassReindex, nil)
	if !r.Predicts() || *r.ExpectedChangePct != -10 {
		t.Fatalf("reindex without a bloat estimate = %+v, want the -10%% rule", r)
	}
	r = predictionFromDetail(verify.ClassReindex, map[string]any{"bloat_pct": 250.0})
	if *r.ExpectedChangePct != -10 {
		t.Fatalf("an impossible bloat estimate (250%%) must not be believed: %v",
			*r.ExpectedChangePct)
	}
}

func TestStatisticsTable(t *testing.T) {
	cases := map[string]string{
		"CREATE STATISTICS s_ab (dependencies) ON a, b FROM public.orders": "public.orders",
		"create statistics if not exists s on a, b from orders;":           "orders",
		`CREATE STATISTICS s ON (lower(a)), b FROM "Sales"."Orders"`:       `"Sales"."Orders"`,
		"CREATE STATISTICS s ON a, b":                                      "",
		"VACUUM t":                                                         "",
	}
	for sql, want := range cases {
		if got := statisticsTable(sql); got != want {
			t.Errorf("statisticsTable(%q) = %q, want %q", sql, got, want)
		}
	}
}
