package earned

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Dogfood round 2 (item 4): statistics and reindex now have verifiers, so
// their outcome classes map to their ledger classes and their verdicts
// earn (or cost) trust like every other self-initiated class. Families
// are unchanged: statistics is tuning (improved only), reindex is hygiene
// (a neutral that held counts).

func TestStatisticsAndReindexHaveOutcomeClasses(t *testing.T) {
	cases := []struct {
		class   ActionClass
		outcome string
		family  Family
	}{
		{ClassStatistics, verify.ClassStatistics, FamilyTuning},
		{ClassReindex, verify.ClassReindex, FamilyHygiene},
	}
	for _, tc := range cases {
		if got := OutcomeClassFor(tc.class); got != tc.outcome {
			t.Errorf("OutcomeClassFor(%s) = %q, want %q", tc.class, got, tc.outcome)
		}
		if got := ClassForOutcomeClass(tc.outcome); got != tc.class {
			t.Errorf("ClassForOutcomeClass(%q) = %q, want %s", tc.outcome, got, tc.class)
		}
		if got := SelfFamilyFor(tc.class); got != tc.family {
			t.Errorf("SelfFamilyFor(%s) = %q, want %q", tc.class, got, tc.family)
		}
	}
	for _, s := range selfClasses {
		if s.outcome == "" {
			t.Errorf("self-initiated class %s still has no verifier: it can never earn",
				s.class)
		}
	}
}

func TestSelfReconcileCountsStatisticsAndReindexVerdicts(t *testing.T) {
	f := newSelfFixture(t)
	stats := f.selfAction("create_statistics",
		"CREATE STATISTICS s_ab (dependencies) ON a, b FROM public.orders", "statistics",
		"success", "improved", "operator_approved", time.Minute)
	statsNeutral := f.selfAction("create_statistics",
		"CREATE STATISTICS s_cd ON c, d FROM public.orders", "statistics", "success",
		"neutral", "operator_approved", time.Minute)
	reindex := f.selfAction("reindex_concurrently",
		"REINDEX INDEX CONCURRENTLY public.orders_a", "reindex", "success", "neutral",
		"operator_approved", time.Minute)
	reindexBad := f.selfAction("reindex_concurrently",
		"REINDEX INDEX CONCURRENTLY public.orders_b", "reindex", "rollback_skipped",
		"regressed", "operator_approved", time.Minute)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.selfOutcomes()
	want := map[int64]selfOutcomeRow{
		stats:        {"tuning", "statistics", "improved", ResultVerifiedRecovery, "executor", 1},
		statsNeutral: {"tuning", "statistics", "neutral", ResultUnverified, "executor", 1},
		reindex:      {"hygiene", "reindex", "neutral", ResultVerifiedRecovery, "executor", 1},
		reindexBad:   {"hygiene", "reindex", "regressed", ResultHarmful, "executor", 1},
	}
	for id, w := range want {
		if len(got[id]) != 1 || got[id][0] != w {
			t.Errorf("action %d: %+v, want [%+v]", id, got[id], w)
		}
	}
	if res.SelfRecorded != 4 {
		t.Fatalf("self recorded = %d, want 4 (%+v)", res.SelfRecorded, res)
	}
}
