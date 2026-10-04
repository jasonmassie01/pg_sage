package verify

import (
	"math"
	"strings"
	"testing"
)

// Dogfood round 2 (item 4): CREATE STATISTICS is verified by what it is
// for, the targeted queries' row estimates (sampled plans with actual
// rows), with their call-weighted time as the harm check.

func TestPlanQError(t *testing.T) {
	cases := []struct {
		name string
		plan string
		want float64
		ok   bool
	}{
		{"explain json array, underestimate",
			`[{"Plan":{"Node Type":"Seq Scan","Plan Rows":1,"Actual Rows":100,
			"Actual Loops":1}}]`, 100, true},
		{"overestimate counts the same",
			`[{"Plan":{"Node Type":"Seq Scan","Plan Rows":400,"Actual Rows":4,
			"Actual Loops":1}}]`, 100, true},
		{"auto_explain object, worst node wins",
			`{"Query Text":"select 1","Plan":{"Node Type":"Hash Join","Plan Rows":10,
			"Actual Rows":20,"Actual Loops":1,"Plans":[{"Node Type":"Seq Scan",
			"Plan Rows":5,"Actual Rows":250,"Actual Loops":1}]}}`, 50, true},
		{"bare node", `{"Node Type":"Seq Scan","Plan Rows":10,"Actual Rows":10,
			"Actual Loops":3}`, 1, true},
		{"zero actual rows clamp to one",
			`[{"Plan":{"Node Type":"Seq Scan","Plan Rows":30,"Actual Rows":0,
			"Actual Loops":1}}]`, 30, true},
		{"pg18 fractional rows",
			`[{"Plan":{"Node Type":"Index Scan","Plan Rows":2,"Actual Rows":250.50,
			"Actual Loops":4}}]`, 125.25, true},
		{"never executed node skipped",
			`[{"Plan":{"Node Type":"Append","Plan Rows":10,"Actual Rows":10,
			"Actual Loops":1,"Plans":[{"Node Type":"Seq Scan","Plan Rows":9000,
			"Actual Rows":0,"Actual Loops":0}]}}]`, 1, true},
		{"nodes under a Limit are skipped",
			`[{"Plan":{"Node Type":"Limit","Plan Rows":10,"Actual Rows":10,
			"Actual Loops":1,"Plans":[{"Node Type":"Seq Scan","Plan Rows":100000,
			"Actual Rows":10,"Actual Loops":1}]}}]`, 1, true},
		{"estimate only (no ANALYZE)",
			`[{"Plan":{"Node Type":"Seq Scan","Plan Rows":10}}]`, 0, false},
		{"malformed json", `[{"Plan":`, 0, false},
		{"empty", ``, 0, false},
		{"empty array", `[]`, 0, false},
		{"not a plan", `{"foo":1}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PlanQError([]byte(tc.plan))
			if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("PlanQError = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSummarizeEstimates(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want EstimateSample
	}{
		{"none", nil, EstimateSample{}},
		{"one", []float64{7}, EstimateSample{Plans: 1, QError: 7}},
		{"odd median", []float64{90, 1, 4}, EstimateSample{Plans: 3, QError: 4}},
		{"even median is geometric", []float64{2, 8, 1, 100},
			EstimateSample{Plans: 4, QError: 4}},
		{"invalid values dropped", []float64{0.5, math.NaN(), math.Inf(1), 3},
			EstimateSample{Plans: 1, QError: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeEstimates(tc.in)
			if got.Plans != tc.want.Plans || math.Abs(got.QError-tc.want.QError) > 1e-9 {
				t.Fatalf("SummarizeEstimates(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func est(plans int, q float64) EstimateSample { return EstimateSample{Plans: plans, QError: q} }

func TestJudgeEstimates(t *testing.T) {
	cases := []struct {
		name          string
		before, after EstimateSample
		verdict       string
		terminal      bool
	}{
		{"halved error is improved (boundary)", est(5, 64), est(5, 32), OutcomeImproved,
			false},
		{"just short of halving is neutral", est(5, 64), est(5, 32.01), OutcomeNeutral,
			false},
		{"just short of doubling is neutral", est(5, 4), est(5, 7.99), OutcomeNeutral,
			false},
		{"fixed estimates", est(5, 100), est(5, 1.2), OutcomeImproved, false},
		{"doubled error is regressed (boundary)", est(5, 4), est(5, 8), OutcomeRegressed,
			false},
		{"accurate before, worse after", est(5, 1.5), est(5, 3), OutcomeRegressed, false},
		{"accurate before and after", est(5, 1.5), est(5, 1.4), OutcomeNeutral, false},
		{"too few plans after", est(5, 100), est(2, 1), OutcomeInsufficient, false},
		{"too few plans before", est(2, 100), est(5, 1), OutcomeInsufficient, false},
		{"no plan with actual rows before", est(0, 0), est(5, 1), OutcomeInsufficient, true},
		{"minimum plans exactly", est(3, 16), est(3, 2), OutcomeImproved, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := JudgeEstimates(tc.before, tc.after, 3)
			if j.Verdict != tc.verdict || j.Terminal != tc.terminal {
				t.Fatalf("JudgeEstimates = %+v, want %s terminal=%v", j, tc.verdict,
					tc.terminal)
			}
			if j.Reason == "" {
				t.Fatal("a judgement must say why")
			}
		})
	}
}

func TestJudgeEstimatesReportsTheChange(t *testing.T) {
	j := JudgeEstimates(est(4, 100), est(4, 2), 0)
	if j.ChangePct == nil || math.Abs(*j.ChangePct+98) > 1e-9 {
		t.Fatalf("change = %v, want -98%% (q 100 -> 2)", j.ChangePct)
	}
	if j.Before != 100 || j.After != 2 {
		t.Fatalf("before/after = %v/%v", j.Before, j.After)
	}
	if !strings.Contains(j.Reason, "100") || !strings.Contains(j.Reason, "2") {
		t.Fatalf("reason %q must name both errors", j.Reason)
	}
	if insufficient := JudgeEstimates(est(1, 100), est(4, 2), 0); insufficient.Verdict !=
		OutcomeInsufficient {
		t.Fatalf("a zero minimum must mean the default (3 plans), got %+v", insufficient)
	}
}

func cmpV(v string) *Comparison { return &Comparison{Verdict: v, Reason: "time " + v} }

func TestDecideStatistics(t *testing.T) {
	ej := func(v string, terminal bool) EstimateJudgement {
		return EstimateJudgement{Verdict: v, Reason: "estimates " + v, Terminal: terminal}
	}
	cases := []struct {
		name     string
		time     *Comparison
		est      EstimateJudgement
		want     string
		accruing bool
	}{
		{"time regression wins", cmpV(OutcomeRegressed), ej(OutcomeImproved, false),
			OutcomeRegressed, false},
		{"estimate regression wins", cmpV(OutcomeImproved), ej(OutcomeRegressed, false),
			OutcomeRegressed, false},
		{"no targeted query", nil, ej(OutcomeImproved, false), OutcomeUnverifiable, false},
		{"estimates fixed, latency held", cmpV(OutcomeNeutral), ej(OutcomeImproved, false),
			OutcomeImproved, false},
		{"estimates fixed, latency faster", cmpV(OutcomeImproved),
			ej(OutcomeImproved, false), OutcomeImproved, false},
		{"estimates fixed, latency unknown", cmpV(OutcomeInsufficient),
			ej(OutcomeImproved, false), OutcomeInsufficient, true},
		{"no plans at all, latency faster", cmpV(OutcomeImproved),
			ej(OutcomeInsufficient, true), OutcomeImproved, false},
		{"no plans at all, latency held", cmpV(OutcomeNeutral),
			ej(OutcomeInsufficient, true), OutcomeNeutral, false},
		{"plans accruing, latency held", cmpV(OutcomeNeutral),
			ej(OutcomeInsufficient, false), OutcomeInsufficient, true},
		{"estimates unchanged, latency held", cmpV(OutcomeNeutral),
			ej(OutcomeNeutral, false), OutcomeNeutral, false},
		{"nothing measurable yet", cmpV(OutcomeInsufficient),
			ej(OutcomeInsufficient, false), OutcomeInsufficient, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, accruing := DecideStatistics(tc.time, tc.est)
			if got != tc.want || accruing != tc.accruing {
				t.Fatalf("DecideStatistics = %s (%s) accruing=%v, want %s accruing=%v",
					got, reason, accruing, tc.want, tc.accruing)
			}
			if reason == "" {
				t.Fatal("a verdict must say why")
			}
		})
	}
}
