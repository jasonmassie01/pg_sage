package slo

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// playbooks-09: the budget window says how much of it the data covers
// (budget_coverage) and, while only part of it is covered, how much
// budget the covered part spent (budget_spent_so_far). A partial window
// is never given a budget_remaining: unknown is never shown as ok.

// No concurrent access tests: Evaluate is a pure function of its inputs.

const budgetWindow = 30 * 24 * time.Hour

// budgetAgg is one stored series whose samples cover the last covered of
// the budget window, with bad and eligible events.
func budgetAgg(covered time.Duration, bad, eligible float64) []SeriesAgg {
	return []SeriesAgg{{Series: "s", Samples: 100, First: now.Add(-covered), Last: now,
		Bad: bad, Eligible: eligible}}
}

func budgetStatus(t *testing.T, o Objective, budget Window) Status {
	t.Helper()
	return Evaluate(o, DefaultRules(), windowsAt(allBurning(0.1)), budget, now)
}

func combined(o Objective, covered time.Duration, bad, eligible float64) Window {
	return Combine(budgetAgg(covered, bad, eligible), o, now.Add(-budgetWindow), now, now)
}

func wantFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}

func TestEvaluate_BudgetAtCoverageThresholdIsKnown(t *testing.T) {
	o := appObjective()
	covered := time.Duration(float64(budgetWindow) * MinCoverage)
	st := budgetStatus(t, o, combined(o, covered, 25, 100000))
	if st.BudgetUnknown != "" {
		t.Fatalf("budget_unknown = %q at exactly %v coverage", st.BudgetUnknown, MinCoverage)
	}
	// 25 bad of 100000 at target 0.999 burns 0.25 of the budget.
	wantFloat(t, "budget_remaining", st.BudgetRemaining, 0.75)
	wantFloat(t, "budget_coverage", st.BudgetCoverage, MinCoverage)
	if st.BudgetSpentSoFar != nil {
		t.Fatalf("budget_spent_so_far = %v on a known budget", *st.BudgetSpentSoFar)
	}
}

func TestEvaluate_BudgetJustUnderThresholdIsPartialWithSpentSoFar(t *testing.T) {
	o := appObjective()
	covered := time.Duration(float64(budgetWindow)*MinCoverage) - time.Second
	st := budgetStatus(t, o, combined(o, covered, 25, 100000))
	if st.BudgetUnknown != ReasonPartialWindow {
		t.Fatalf("budget_unknown = %q, want %q", st.BudgetUnknown, ReasonPartialWindow)
	}
	if st.BudgetRemaining != nil {
		t.Fatalf("budget_remaining = %v on a partial window", *st.BudgetRemaining)
	}
	wantFloat(t, "budget_spent_so_far", st.BudgetSpentSoFar, 0.25)
	if st.BudgetCoverage == nil || *st.BudgetCoverage >= MinCoverage ||
		*st.BudgetCoverage < MinCoverage-0.001 {
		t.Fatalf("budget_coverage = %v, want just under %v", st.BudgetCoverage, MinCoverage)
	}
}

func TestEvaluate_BudgetPartialWindowReportsCoveredSpend(t *testing.T) {
	o := appObjective()
	st := budgetStatus(t, o, combined(o, 3*24*time.Hour, 50, 100000))
	if st.BudgetUnknown != ReasonPartialWindow || st.BudgetRemaining != nil {
		t.Fatalf("budget = %v / %q, want unknown partial_window", st.BudgetRemaining,
			st.BudgetUnknown)
	}
	wantFloat(t, "budget_coverage", st.BudgetCoverage, 0.1)
	// (50 / 100000) / (1 - 0.999) = 0.5 of the budget spent in the covered tenth.
	wantFloat(t, "budget_spent_so_far", st.BudgetSpentSoFar, 0.5)
}

func TestEvaluate_BudgetZeroEligibleHasNoSpentSoFar(t *testing.T) {
	o := appObjective()
	full := budgetStatus(t, o, combined(o, budgetWindow, 0, 0))
	if full.BudgetUnknown != ReasonZeroEligible || full.BudgetRemaining != nil {
		t.Fatalf("full window: budget = %v / %q, want zero_eligible", full.BudgetRemaining,
			full.BudgetUnknown)
	}
	if full.BudgetSpentSoFar != nil {
		t.Fatalf("full window: spent so far = %v with zero eligible", *full.BudgetSpentSoFar)
	}
	wantFloat(t, "budget_coverage", full.BudgetCoverage, 1)
	partial := budgetStatus(t, o, combined(o, 24*time.Hour, 0, 0))
	if partial.BudgetUnknown != ReasonPartialWindow {
		t.Fatalf("partial window: budget_unknown = %q", partial.BudgetUnknown)
	}
	if partial.BudgetSpentSoFar != nil {
		t.Fatalf("partial window: spent so far = %v with zero eligible",
			*partial.BudgetSpentSoFar)
	}
}

func TestEvaluate_BudgetPartialLowTrafficHasNoSpentSoFar(t *testing.T) {
	o := appObjective() // min_eligible 100
	st := budgetStatus(t, o, combined(o, 24*time.Hour, 3, 99))
	if st.BudgetUnknown != ReasonPartialWindow {
		t.Fatalf("budget_unknown = %q", st.BudgetUnknown)
	}
	if st.BudgetSpentSoFar != nil {
		t.Fatalf("spent so far = %v from 99 events, under min_eligible_events",
			*st.BudgetSpentSoFar)
	}
	at := budgetStatus(t, o, combined(o, 24*time.Hour, 1, 100))
	// (1 / 100) / 0.001 = 10x the budget in the covered day.
	wantFloat(t, "budget_spent_so_far at min_eligible", at.BudgetSpentSoFar, 10)
}

func TestEvaluate_BudgetInvalidOrUnreadHasNoCoverage(t *testing.T) {
	o := appObjective()
	invalid := budgetStatus(t, o, combined(o, 24*time.Hour, 200, 100))
	if invalid.BudgetUnknown != ReasonInvalidValue {
		t.Fatalf("bad > eligible: budget_unknown = %q", invalid.BudgetUnknown)
	}
	if invalid.BudgetSpentSoFar != nil || invalid.BudgetCoverage != nil {
		t.Fatalf("invalid counts: spent %v coverage %v, want none",
			invalid.BudgetSpentSoFar, invalid.BudgetCoverage)
	}
	cases := map[string]Window{
		"no data":      Combine(nil, o, now.Add(-budgetWindow), now, now),
		"source error": {Duration: budgetWindow, Coverage: 1, Unknown: ReasonSourceError},
		// The newest sample is an hour old; the objective is stale after 5 minutes.
		"stale": Combine([]SeriesAgg{{Series: "s", Samples: 10,
			First: now.Add(-budgetWindow), Last: now.Add(-time.Hour), Bad: 1,
			Eligible: 1000}}, o, now.Add(-budgetWindow), now, now),
	}
	for name, w := range cases {
		st := budgetStatus(t, o, w)
		if st.BudgetUnknown == "" || st.BudgetRemaining != nil {
			t.Errorf("%s: budget = %v / %q, want unknown", name, st.BudgetRemaining,
				st.BudgetUnknown)
		}
		if st.BudgetCoverage != nil || st.BudgetSpentSoFar != nil {
			t.Errorf("%s: coverage %v spent %v, want none for a window not read",
				name, st.BudgetCoverage, st.BudgetSpentSoFar)
		}
	}
}

func TestEvaluate_BudgetCoverageUnverifiedIsNotClaimed(t *testing.T) {
	o := appObjective()
	o.Source, o.EligibleQuery, o.BadQuery = SourcePrometheus, "e[$window]", "b[$window]"
	st := budgetStatus(t, o, known(budgetWindow, 0.25))
	if !st.CoverageUnverified {
		t.Fatal("a Prometheus SLO without coverage_query is not coverage_unverified")
	}
	wantFloat(t, "budget_remaining", st.BudgetRemaining, 0.75)
	if st.BudgetCoverage != nil {
		t.Fatalf("budget_coverage = %v for an unverified Prometheus window",
			*st.BudgetCoverage)
	}
}

func TestEvaluate_BudgetFieldsInJSON(t *testing.T) {
	o := appObjective()
	raw, err := json.Marshal(budgetStatus(t, o, combined(o, 3*24*time.Hour, 50, 100000)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["budget_unknown"] != ReasonPartialWindow || got["budget_remaining"] != nil {
		t.Fatalf("budget = %v / %v", got["budget_remaining"], got["budget_unknown"])
	}
	if c, ok := got["budget_coverage"].(float64); !ok || math.Abs(c-0.1) > 1e-9 {
		t.Fatalf("budget_coverage = %v", got["budget_coverage"])
	}
	if s, ok := got["budget_spent_so_far"].(float64); !ok || math.Abs(s-0.5) > 1e-9 {
		t.Fatalf("budget_spent_so_far = %v", got["budget_spent_so_far"])
	}
	raw, err = json.Marshal(budgetStatus(t, o, Window{Duration: budgetWindow,
		Unknown: ReasonNoData}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"budget_coverage", "budget_spent_so_far"} {
		if _, present := got[k]; present {
			t.Errorf("%s present for a window with no data: %v", k, got[k])
		}
	}
}
