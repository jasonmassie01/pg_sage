package tuner

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/planfresh"
)

// No concurrent access tests: estimateSymptoms and planMaxAge are pure.

func TestTuner_PlanMaxAgeDefaults(t *testing.T) {
	cases := map[int]time.Duration{0: 24 * time.Hour, -1: 24 * time.Hour,
		90: 90 * time.Minute}
	for minutes, want := range cases {
		tu := New(nil, TunerConfig{PlanMaxAgeMinutes: minutes}, nil, noopLogFn)
		if got := tu.planMaxAge(); got != want {
			t.Errorf("plan_max_age_minutes=%d: %v, want %v", minutes, got, want)
		}
	}
}

func TestEstimateSymptoms_DropsActualsOnlyRules(t *testing.T) {
	in := []PlanSymptom{{Kind: SymptomDiskSort}, {Kind: SymptomHashSpill},
		{Kind: SymptomBadNestedLoop}, {Kind: SymptomSeqScanWithIndex},
		{Kind: SymptomSortLimit}}
	out := estimateSymptoms(in)
	if len(out) != 2 || out[0].Kind != SymptomSeqScanWithIndex ||
		out[1].Kind != SymptomSortLimit {
		t.Fatalf("estimate-only plan keeps only estimate rules: %v", out)
	}
	if got := estimateSymptoms(nil); len(got) != 0 {
		t.Fatalf("nil in, nothing out: %v", got)
	}
}

func TestTuner_NoPoolHasNoPlan(t *testing.T) {
	tu := New(nil, TunerConfig{}, nil, noopLogFn)
	if p := tu.cyclePlan(t.Context(), 1); p.Source != planfresh.SourceNone ||
		len(p.JSON) != 0 {
		t.Fatalf("no pool: no plan, got %+v", p)
	}
}
