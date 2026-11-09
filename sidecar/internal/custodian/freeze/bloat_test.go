package freeze

import (
	"math"
	"testing"
)

// vacuum-04: a bloat plan needs the catalog bloat estimate (never the
// dead-tuple ratio) at or above 0.50 on a table at least the size floor.
// No concurrency tests: PlanResponse and EstimatedBloat are pure functions.

const gib = int64(1) << 30

func bloatInput(ratio float64, size int64) ResponseInput {
	return ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyGreen,
		BloatRatio: ratio, BloatKnown: true, SizeBytes: size, MinBloatBytes: gib,
		BloatSource: BloatSourceEstimate}
}

func TestPlanResponseBloatNeedsEstimateAndSizeFloor(t *testing.T) {
	tests := []struct {
		name string
		in   ResponseInput
		kind ResponseKind
	}{
		{"estimate above threshold on a large table", bloatInput(0.6, 2*gib),
			ResponseParkBloat},
		{"ratio 0.50 is a plan", bloatInput(0.50, 2*gib), ResponseParkBloat},
		{"ratio 0.49 is not", bloatInput(0.49, 2*gib), ""},
		{"size exactly at the floor is a plan", bloatInput(0.7, gib), ResponseParkBloat},
		{"size one byte under the floor is not", bloatInput(0.7, gib-1), ""},
		{"size zero is not", bloatInput(0.9, 0), ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := PlanResponse(test.in)
			if err != nil {
				t.Fatalf("PlanResponse: %v", err)
			}
			if response.Kind != test.kind {
				t.Fatalf("kind = %q, want %q (response %#v)", response.Kind, test.kind, response)
			}
			if test.kind == "" && (response.Plan != "" || response.SQL != "") {
				t.Fatalf("no-plan case produced %#v", response)
			}
			if test.kind != "" && (response.Plan == "" || response.SQL != "") {
				t.Fatalf("bloat plan must carry a plan and no SQL: %#v", response)
			}
		})
	}
}

func TestPlanResponseDeadTuplesAloneAreNotBloat(t *testing.T) {
	// A 2-row table with 1 dead row: dead ratio 0.5, no estimate, tiny.
	in := ResponseInput{Schema: "public", Table: "meta", Urgency: UrgencyGreen,
		DeadTupleRatio: 0.5, SizeBytes: 8192, MinBloatBytes: gib}
	for _, known := range []bool{false, true} {
		in.BloatKnown = known
		response, err := PlanResponse(in)
		if err != nil {
			t.Fatalf("PlanResponse: %v", err)
		}
		if response.Kind != "" || response.Plan != "" {
			t.Fatalf("dead tuples became a bloat plan (known=%v): %#v", known, response)
		}
	}
	// Even a fully dead large table is the analyzer's VACUUM, not a rebuild.
	in = ResponseInput{Schema: "public", Table: "big", Urgency: UrgencyGreen,
		DeadTupleRatio: 1, SizeBytes: 4 * gib, MinBloatBytes: gib}
	if response, err := PlanResponse(in); err != nil || response.Kind != "" {
		t.Fatalf("dead ratio without an estimate planned %#v (%v)", response, err)
	}
}

func TestPlanResponseBloatNilAndZero(t *testing.T) {
	cases := map[string]ResponseInput{
		"0 live and 0 dead rows": {Schema: "public", Table: "empty", MinBloatBytes: gib},
		"size 0 with an estimate": {Schema: "public", Table: "empty", BloatKnown: true,
			BloatRatio: 1, MinBloatBytes: gib},
		// reltuples = -1: the catalog estimate is NULL, so BloatKnown is false.
		"never analyzed": {Schema: "public", Table: "fresh", BloatRatio: 0.9,
			SizeBytes: 8 * gib, MinBloatBytes: gib},
		// No floor configured: fail closed rather than plan for every table.
		"zero floor": {Schema: "public", Table: "orders", BloatKnown: true,
			BloatRatio: 0.9, SizeBytes: 8 * gib},
	}
	for name, in := range cases {
		response, err := PlanResponse(in)
		if err != nil {
			t.Fatalf("%s: PlanResponse: %v", name, err)
		}
		if response.Kind != "" || response.Plan != "" || response.SQL != "" {
			t.Fatalf("%s: produced %#v", name, response)
		}
	}
}

func TestPlanResponseBloatPlanUsesRepackOnlyWhenAvailable(t *testing.T) {
	in := bloatInput(0.8, 3*gib)
	in.PGRepackAvailable = true
	response, err := PlanResponse(in)
	if err != nil || response.Kind != ResponsePlanRepack {
		t.Fatalf("repack plan = %#v (%v)", response, err)
	}
	want := map[string]any{"bloat_ratio": 0.8, "bloat_source": BloatSourceEstimate,
		"heap_bytes": 3 * gib, "bloat_min_bytes": gib, "bloat_threshold": BloatPlanThreshold}
	for key, value := range want {
		if response.Evidence[key] != value {
			t.Fatalf("evidence[%s] = %#v, want %#v (%#v)", key,
				response.Evidence[key], value, response.Evidence)
		}
	}
}

func TestPlanResponseRejectsNegativeBloatSizes(t *testing.T) {
	for _, in := range []ResponseInput{
		{Schema: "public", Table: "orders", SizeBytes: -1},
		{Schema: "public", Table: "orders", MinBloatBytes: -1},
		{Schema: "public", Table: "orders", BloatRatio: -0.1},
		{Schema: "public", Table: "orders", BloatRatio: 1.01},
	} {
		if _, err := PlanResponse(in); err == nil {
			t.Fatalf("invalid bloat input accepted: %#v", in)
		}
	}
}

func TestEstimatedBloat(t *testing.T) {
	half, quarter, over, zero := 50.0, 75.0, 140.0, 0.0
	nan := math.NaN()
	tests := []struct {
		name     string
		pages    int64
		expected *float64
		ratio    float64
		known    bool
	}{
		{"half the pages are needed", 100, &half, 0.5, true},
		{"three quarters needed", 100, &quarter, 0.25, true},
		{"more needed than present clamps to 0", 100, &over, 0, true},
		{"no rows needed clamps to 1", 100, &zero, 1, true},
		{"no estimate (never analyzed or no stats)", 100, nil, 0, false},
		{"no pages", 0, &half, 0, false},
		{"negative pages", -5, &half, 0, false},
		{"NaN estimate", 100, &nan, 0, false},
	}
	for _, test := range tests {
		ratio, known := EstimatedBloat(test.pages, test.expected)
		if known != test.known || math.Abs(ratio-test.ratio) > 1e-9 {
			t.Fatalf("%s: EstimatedBloat = %v, %v; want %v, %v",
				test.name, ratio, known, test.ratio, test.known)
		}
	}
}
