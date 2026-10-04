package verify

import (
	"math"
	"testing"
)

// Dogfood round 2 (item 4): REINDEX is verified by the index's size (the
// bloat it reclaimed), its validity, and no regression of the queries on
// its table. It is hygiene: a rebuild that held without harm counts.

func TestJudgeIndexSize(t *testing.T) {
	cases := []struct {
		name          string
		before, after int64
		valid         bool
		verdict       string
		change        float64
	}{
		{"shrank 10% exactly", 1000, 900, true, OutcomeImproved, -10},
		{"shrank just under 10%", 1000, 901, true, OutcomeNeutral, -9.9},
		{"shrank 60%", 100 << 20, 40 << 20, true, OutcomeImproved, -60},
		{"same size", 8192 * 10, 8192 * 10, true, OutcomeNeutral, 0},
		{"grew", 8192 * 10, 8192 * 11, true, OutcomeNeutral, 10},
		{"invalid after rebuild", 1000, 500, false, OutcomeRegressed, -50},
		{"size unknown before", 0, 500, true, OutcomeUnverifiable, 0},
		{"unreadable after", 1000, -1, true, OutcomeUnverifiable, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := JudgeIndexSize(tc.before, tc.after, tc.valid)
			if j.Verdict != tc.verdict {
				t.Fatalf("JudgeIndexSize = %+v, want %s", j, tc.verdict)
			}
			if j.Reason == "" {
				t.Fatal("a judgement must say why")
			}
			if tc.verdict == OutcomeUnverifiable {
				if j.ChangePct != nil {
					t.Fatalf("unmeasurable size reported a change %v", *j.ChangePct)
				}
				return
			}
			if j.ChangePct == nil || math.Abs(*j.ChangePct-tc.change) > 1e-9 {
				t.Fatalf("change = %v, want %v", j.ChangePct, tc.change)
			}
		})
	}
}

func TestDecideReindex(t *testing.T) {
	size := func(v string) SizeJudgement { return SizeJudgement{Verdict: v, Reason: "size " + v} }
	cases := []struct {
		name     string
		time     *Comparison
		size     SizeJudgement
		want     string
		accruing bool
	}{
		{"query regression wins", cmpV(OutcomeRegressed), size(OutcomeImproved),
			OutcomeRegressed, false},
		{"invalid index", cmpV(OutcomeNeutral), size(OutcomeRegressed), OutcomeRegressed,
			false},
		{"size unreadable", cmpV(OutcomeNeutral), size(OutcomeUnverifiable),
			OutcomeUnverifiable, false},
		{"shrank, reads held", cmpV(OutcomeNeutral), size(OutcomeImproved),
			OutcomeImproved, false},
		{"shrank, reads faster", cmpV(OutcomeImproved), size(OutcomeImproved),
			OutcomeImproved, false},
		{"held size, reads held", cmpV(OutcomeNeutral), size(OutcomeNeutral),
			OutcomeNeutral, false},
		{"shrank, reads not yet measurable", cmpV(OutcomeInsufficient),
			size(OutcomeImproved), OutcomeInsufficient, true},
		{"no statement reads the table", nil, size(OutcomeImproved), OutcomeImproved,
			false},
		{"no statement, size held", nil, size(OutcomeNeutral), OutcomeNeutral, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, accruing := DecideReindex(tc.time, tc.size)
			if got != tc.want || accruing != tc.accruing {
				t.Fatalf("DecideReindex = %s (%s) accruing=%v, want %s accruing=%v", got,
					reason, accruing, tc.want, tc.accruing)
			}
			if reason == "" {
				t.Fatal("a verdict must say why")
			}
		})
	}
}

func TestReindexAndStatisticsAreVerifiedClasses(t *testing.T) {
	if ClassStatistics != "statistics" || ClassReindex != "reindex" {
		t.Fatalf("class names %q/%q: sage.action_outcome rows and the trust ledger key "+
			"on them", ClassStatistics, ClassReindex)
	}
	if MetricRowEstimateError == "" || MetricIndexBytes == "" ||
		MetricRowEstimateError == MetricIndexBytes {
		t.Fatal("each class needs its own metric name")
	}
}

// No concurrent access tests: the judges are pure functions.
