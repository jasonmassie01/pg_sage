package causal

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Causal graph v1, plan regression family: a query_store.plan_hash flip
// joined with the windowed latency change. A latency regression without
// a plan change is a competing hypothesis, and each rules the other out.

func shiftRow(qid int64, flipped bool, before, after float64, calls int64) probes.Row {
	row := probes.Row{"queryid": qid, "current_plan_hash": "v1:new",
		"plan_flipped": flipped, "before_calls": calls, "before_mean_ms": before,
		"after_calls": calls, "after_mean_ms": after,
		"previous_plan_hash": nil, "flipped_at": nil}
	if flipped {
		row["previous_plan_hash"] = "v1:old"
		row["flipped_at"] = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	}
	return row
}

func planObs(rows ...probes.Row) []Observation {
	return []Observation{obs("P3", probes.PlanRegressions, rows...)}
}

func TestDiagnosePlan_FlipRegression(t *testing.T) {
	ds := DiagnosePlan(planObs(shiftRow(42, true, 0.5, 5, 20)))
	if len(ds) != 1 {
		t.Fatalf("diagnoses = %d, want 1", len(ds))
	}
	d := ds[0]
	root := requireRoot(t, d, PlanFlipRegression, 0.90)
	if root.Subject != "queryid 42" || d.Family != FamilyPlanRegression {
		t.Fatalf("subject/family = %q/%s", root.Subject, d.Family)
	}
	joined := ""
	for _, f := range root.Support {
		joined += f.Text + "|"
	}
	for _, want := range []string{"v1:old", "v1:new", "0.5", "5", "10"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("support %q lacks %q", joined, want)
		}
	}
	citesOnly(t, root, "P3")
	requireRuledOut(t, d, SamePlanLatencyRegression, "plan changed")
}

func TestDiagnosePlan_SamePlanRegression(t *testing.T) {
	ds := DiagnosePlan(planObs(shiftRow(43, false, 1, 3, 10)))
	if len(ds) != 1 {
		t.Fatalf("diagnoses = %d, want 1", len(ds))
	}
	requireRoot(t, ds[0], SamePlanLatencyRegression, 0.70)
	requireRuledOut(t, ds[0], PlanFlipRegression, "plan_hash unchanged")
}

func TestDiagnosePlan_RegressionRatioBoundary(t *testing.T) {
	cases := []struct {
		after float64
		want  int
	}{{1.49, 0}, {1.5, 1}, {1.51, 1}}
	for _, c := range cases {
		ds := DiagnosePlan(planObs(shiftRow(44, true, 1, c.after, 20)))
		if len(ds) != c.want {
			t.Errorf("ratio %.2f -> %d diagnoses, want %d", c.after, len(ds), c.want)
		}
	}
}

func TestDiagnosePlan_FlipWithoutRegressionIsNotAnIncident(t *testing.T) {
	if ds := DiagnosePlan(planObs(shiftRow(45, true, 2, 2.1, 20))); len(ds) != 0 {
		t.Fatalf("a benign plan change produced %+v", ds)
	}
	if ds := DiagnosePlan(planObs(shiftRow(46, true, 2, 0.5, 20))); len(ds) != 0 {
		t.Fatalf("a faster plan produced %+v", ds)
	}
}

func TestDiagnosePlan_UnknownBaselineIsNotARegression(t *testing.T) {
	if ds := DiagnosePlan(planObs(shiftRow(47, true, 0, 5, 20))); len(ds) != 0 {
		t.Fatalf("zero baseline latency (unknown ratio) produced %+v", ds)
	}
}

func TestDiagnosePlan_SmallSamplesScoreLower(t *testing.T) {
	ds := DiagnosePlan(planObs(shiftRow(48, true, 1, 2, 1)))
	if len(ds) != 1 {
		t.Fatalf("diagnoses = %d", len(ds))
	}
	requireRoot(t, ds[0], PlanFlipRegression, 0.70)
}

func TestDiagnosePlan_UnavailableProbeIsReportedMissing(t *testing.T) {
	ds := DiagnosePlan([]Observation{unavailable("P3", probes.PlanRegressions,
		probes.StatusNoPrivilege)})
	if len(ds) != 1 || ds[0].Conclusive || len(ds[0].Missing) != 1 ||
		ds[0].Missing[0].Status != probes.StatusNoPrivilege {
		t.Fatalf("diagnoses = %+v, want one inconclusive with missing evidence", ds)
	}
	if ds := DiagnosePlan(nil); len(ds) != 1 || ds[0].Conclusive ||
		ds[0].Missing[0].Reason != "not_collected" {
		t.Fatalf("no observation = %+v, want inconclusive not_collected", ds)
	}
}

func TestDiagnosePlan_OrderedByRatioAndCapped(t *testing.T) {
	var rows []probes.Row
	for i := 0; i < MaxPlanDiagnoses+3; i++ {
		rows = append(rows, shiftRow(int64(100+i), true, 1, float64(2+i), 20))
	}
	ds := DiagnosePlan(planObs(rows...))
	if len(ds) != MaxPlanDiagnoses {
		t.Fatalf("diagnoses = %d, want %d", len(ds), MaxPlanDiagnoses)
	}
	if ds[0].Root.Subject != "queryid 107" {
		t.Fatalf("first diagnosis = %s, want the largest regression", ds[0].Root.Subject)
	}
	for i := 1; i < len(ds); i++ {
		if ratioOf(ds[i-1]) < ratioOf(ds[i]) {
			t.Fatal("diagnoses not ordered by regression ratio")
		}
	}
}

func ratioOf(d Diagnosis) float64 {
	if d.Root == nil {
		return math.NaN()
	}
	return d.Ratio
}
