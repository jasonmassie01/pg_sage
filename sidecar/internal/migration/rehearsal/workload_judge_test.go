package rehearsal

import (
	"strings"
	"testing"
)

// lab-04b: the judge on a measured workload. No concurrent access tests
// here: judge is a pure function of one measurement (concurrency is in
// receipt_test.go).

func judgeOf(m Measurement) Result { return NewOrchestrator(nil, nil, testOptions()).judge(m) }

func int64p(v int64) *int64 { return &v }

// Happy path: an Index Scan to Seq Scan flip on a planned-only statement
// parks on its plan cost, and the detail names the statement.
func TestJudgeParksPlannedCostFlip(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{{
		QueryID: 11, BeforeCost: 8.3, AfterCost: 4180, BeforePlanHash: "v2u:index",
		AfterPlanHash: "v2u:seq", NotExecuted: "not executed: parameters unknown",
	}}})
	if got.Verdict != VerdictPark || got.Reason != ReasonPlanRegression {
		t.Fatalf("result = %#v", got)
	}
	for _, want := range []string{"queryid 11", "cost", "8.300", "4180.000", "plan changed"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("detail %q does not name %q", got.Detail, want)
		}
	}
}

// Happy path: an unchanged plan whose cost held promotes on plan cost.
func TestJudgePromotesUnchangedPlannedStatement(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{{
		QueryID: 12, BeforeCost: 8.3, AfterCost: 8.3, BeforePlanHash: "v2u:index",
		AfterPlanHash: "v2u:index", NotExecuted: "not executed: parameters unknown",
	}}})
	if got.Verdict != VerdictPromoteExpand || got.Reason != ReasonRehearsalPassed ||
		got.Detail != "" {
		t.Fatalf("result = %#v", got)
	}
}

// Boundary: a latency rise at or under the 1 ms noise floor is not a
// regression even when the plan flipped; just over it parks.
func TestJudgeLatencyNoiseFloor(t *testing.T) {
	for _, test := range []struct {
		after float64
		park  bool
	}{{2.0, false}, {1.5, false}, {2.01, true}} {
		got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{{
			QueryID: 3, BeforeLatencyMS: 1, AfterLatencyMS: test.after,
			BeforePlanHash: "a", AfterPlanHash: "b",
		}}})
		if (got.Verdict == VerdictPark) != test.park {
			t.Fatalf("1 ms to %v ms: %#v, want park=%v", test.after, got, test.park)
		}
	}
}

// Nil/empty: the two empty workloads stay inconclusive with distinct
// reasons.
func TestJudgeEmptyWorkloadNamesWhy(t *testing.T) {
	for _, note := range []string{"no statement references the table",
		"pg_stat_statements unavailable on production: extension is not installed"} {
		got := judgeOf(Measurement{WorkloadNote: note})
		if got.Verdict != VerdictRecommendOnly || got.Reason != ReasonInconclusiveWorkload ||
			got.Detail != note {
			t.Fatalf("note %q: result = %#v", note, got)
		}
	}
}

// Invalid input and boundary: a multi-statement text and a statement that
// timed out are unmeasured; with nothing else measured the result is
// inconclusive, never a park, and the detail names the first gap.
func TestJudgeUnmeasuredOnlyIsInconclusive(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{
		{QueryID: 5, Unmeasured: UnmeasuredNotOne},
		{QueryID: 6, Unmeasured: UnmeasuredTimedOutBefore, TimedOut: true},
	}})
	if got.Verdict != VerdictRecommendOnly || got.Reason != ReasonInconclusiveWorkload {
		t.Fatalf("result = %#v", got)
	}
	if !strings.Contains(got.Detail, "queryid 5: "+UnmeasuredNotOne) {
		t.Fatalf("detail = %q", got.Detail)
	}
}

// Boundary: a skipped and a timed-out statement do not stop a measured
// one from promoting.
func TestJudgeUnmeasuredDoesNotBlockMeasured(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{
		{QueryID: 5, Unmeasured: UnmeasuredNotOne},
		{QueryID: 6, Unmeasured: UnmeasuredTimedOutBefore, TimedOut: true},
		{QueryID: 7, BeforeLatencyMS: 4, AfterLatencyMS: 4.1, BeforePlanHash: "a",
			AfterPlanHash: "a", BeforeBlocks: int64p(3), AfterBlocks: int64p(3)},
	}})
	if got.Verdict != VerdictPromoteExpand {
		t.Fatalf("result = %#v", got)
	}
}

// Safety: a statement that ran before the expand steps and timed out
// after them is no regression evidence, but it never lets the rehearsal
// promote.
func TestJudgeTimeoutAfterStepsNeverPromotes(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{
		{QueryID: 7, BeforeLatencyMS: 4, AfterLatencyMS: 4, BeforePlanHash: "a",
			AfterPlanHash: "a"},
		{QueryID: 8, BeforeLatencyMS: 40, Unmeasured: UnmeasuredTimedOutAfter,
			TimedOut: true},
	}})
	if got.Verdict != VerdictRecommendOnly || got.Reason != ReasonInconclusiveWorkload ||
		!strings.Contains(got.Detail, "queryid 8 timed out") {
		t.Fatalf("result = %#v", got)
	}
}

// A latency regression's detail names the statement and the change.
func TestJudgeLatencyRegressionDetail(t *testing.T) {
	got := judgeOf(Measurement{AffectedQueries: []QueryMeasurement{{
		QueryID: 9, BeforeLatencyMS: 10, AfterLatencyMS: 30, BeforePlanHash: "a",
		AfterPlanHash: "a",
	}}})
	if got.Verdict != VerdictPark || got.Detail != "queryid 9: ms 10.000 to 30.000 (+200.0%)" {
		t.Fatalf("result = %#v", got)
	}
}
