package rehearsal

import "testing"

// No concurrent access tests: judge and regressed are pure functions of
// one measurement and the options (migration-06 part A).

// TestRegressedBoundaries is CHECK-26: a plan-hash flip parks at a lower
// latency change (PlanFlipPct, default 5) than a same-plan regression
// (RegressionPct). Before migration-06 the flip clause repeated the
// RegressionPct test, so the +5.1% flip row passed.
func TestRegressedBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		after float64
		flip  bool
		park  bool
	}{
		{"same plan +14.9% passes", 114.9, false, false},
		{"same plan +15.1% parks", 115.1, false, true},
		{"same plan +15.0% passes (strictly greater)", 115, false, false},
		{"plan flip +5.1% parks", 105.1, true, true},
		{"plan flip +4.9% passes", 104.9, true, false},
		{"plan flip faster passes", 90, true, false},
		{"plan flip +15.1% parks", 115.1, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			afterHash := "plan-a"
			if test.flip {
				afterHash = "plan-b"
			}
			measurement := Measurement{AffectedQueries: []QueryMeasurement{{
				QueryID: 7, BeforeLatencyMS: 100, AfterLatencyMS: test.after,
				BeforePlanHash: "plan-a", AfterPlanHash: afterHash,
			}}}
			result := NewOrchestrator(nil, nil, testOptions()).judge(measurement)
			want := VerdictPromoteExpand
			if test.park {
				want = VerdictPark
			}
			if result.Verdict != want {
				t.Fatalf("verdict = %q (reason %q), want %q", result.Verdict,
					result.Reason, want)
			}
			if test.park && result.Reason != ReasonPlanRegression {
				t.Fatalf("reason = %q, want %q", result.Reason, ReasonPlanRegression)
			}
		})
	}
}

// A query with no baseline latency is not evidence either way: it never
// parks, and with no other statement measured it cannot promote either
// (lab-04b: before, this test expected a promote on a workload with no
// measured statement, which is the vacuous pass G7-B22 forbids).
func TestRegressedIgnoresZeroBaseline(t *testing.T) {
	measurement := Measurement{AffectedQueries: []QueryMeasurement{{
		QueryID: 7, BeforeLatencyMS: 0, AfterLatencyMS: 500,
		BeforePlanHash: "plan-a", AfterPlanHash: "plan-b",
	}}}
	if got := NewOrchestrator(nil, nil, testOptions()).judge(measurement); got.Verdict !=
		VerdictRecommendOnly || got.Reason != ReasonInconclusiveWorkload {
		t.Fatalf("zero baseline: %#v", got)
	}
}

func TestJudgeEmptyWorkloadIsInconclusive(t *testing.T) {
	for name, queries := range map[string][]QueryMeasurement{
		"nil": nil, "empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			got := NewOrchestrator(nil, nil, testOptions()).judge(
				Measurement{AffectedQueries: queries})
			if got.Verdict != VerdictRecommendOnly ||
				got.Reason != ReasonInconclusiveWorkload {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}
