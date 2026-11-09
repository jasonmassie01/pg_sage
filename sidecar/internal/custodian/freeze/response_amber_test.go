package freeze

import (
	"strings"
	"testing"
)

// vacuum-07: the analyzer is the single autovacuum tuner. An amber table
// gets no custodian response at any dead-tuple ratio (before, a ratio of
// at least 0.2 re-applied scale factor 0.02 every cycle and fought the
// analyzer's 0.01 on large tables). No concurrency test: PlanResponse is
// a pure function of its input.

func amberInput(dead float64) ResponseInput {
	return ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyAmber,
		XIDAge: 120_000_000, DeadTupleRatio: dead}
}

func TestPlanResponseAmberEmitsNothing(t *testing.T) {
	for _, dead := range []float64{0, 0.19, 0.2, 0.3, 1} {
		response, err := PlanResponse(amberInput(dead))
		if err != nil {
			t.Fatalf("dead %v: PlanResponse: %v", dead, err)
		}
		if response.Kind != "" || response.SQL != "" || response.Plan != "" ||
			response.PinnedBy != nil || response.Evidence != nil {
			t.Fatalf("dead %v: amber response = %#v, want none", dead, response)
		}
	}
}

// An amber table with a known bloat estimate still gets its bloat plan:
// only the tuning rung is gone, never an ALTER TABLE.
func TestPlanResponseAmberBloatStillPlans(t *testing.T) {
	input := amberInput(0.3)
	input.BloatRatio, input.BloatKnown = 0.6, true
	input.SizeBytes, input.MinBloatBytes = 2<<30, 1<<30
	response, err := PlanResponse(input)
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if response.Kind != ResponseParkBloat || response.Plan == "" || response.SQL != "" {
		t.Fatalf("amber bloat response = %#v, want a parked bloat plan", response)
	}
}

// Every response kind the custodian can still return; none is a tuner.
func TestPlanResponseNeverAltersTables(t *testing.T) {
	inputs := []ResponseInput{amberInput(0.5), redInput(1000, nil),
		{Schema: "public", Table: "orders", Urgency: UrgencyGreen, DeadTupleRatio: 0.9}}
	for _, input := range inputs {
		response, err := PlanResponse(input)
		if err != nil {
			t.Fatalf("PlanResponse(%#v): %v", input, err)
		}
		if strings.Contains(strings.ToUpper(response.SQL), "ALTER TABLE") ||
			string(response.Kind) == "tune_autovacuum" {
			t.Fatalf("custodian tuned a table: %#v", response)
		}
	}
}

// Invalid input still fails before any rung is chosen.
func TestPlanResponseAmberRejectsOutOfRangeRatio(t *testing.T) {
	for _, dead := range []float64{-0.01, 1.01} {
		if _, err := PlanResponse(amberInput(dead)); err == nil ||
			!strings.Contains(err.Error(), "ratios") {
			t.Fatalf("dead %v: err = %v, want a ratio range error", dead, err)
		}
	}
}
