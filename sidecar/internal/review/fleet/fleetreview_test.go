package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Tier 3: one fleet-level review whose answers are computed in code; the
// model only writes the summary.

func fleetFixture() []Rollup {
	usd := func(v float64) *float64 { return &v }
	rs := []Rollup{
		{Database: "a", Graded: true, IdleKnown: true, Idle: true, QuietDays: 20,
			SizeBytes: 10 << 30, MonthlyUSD: usd(12.5)},
		{Database: "b", Graded: true, IdleKnown: true, Idle: true, QuietDays: 15},
		{Database: "c", Graded: true, IdleKnown: true, Fingerprint: "fp1",
			Kinds: []string{"indexes/unused_index", "config/work_mem"}},
		{Database: "d", Graded: true, IdleKnown: true, Fingerprint: "fp1",
			Kinds: []string{"indexes/unused_index"}},
		{Database: "e", Graded: true, IdleKnown: false, Fingerprint: "fp2",
			Kinds: []string{"indexes/unused_index"}},
	}
	for i := range rs {
		rs[i].ID = fmt.Sprintf("R%d", i+1)
	}
	return rs
}

func TestReviewFleet_AnswersTheFleetQuestionsInCode(t *testing.T) {
	m := constModel("Two databases look abandoned.", nil)
	fr := ReviewFleet(context.Background(), m, fleetFixture(), nil)
	if fr.Skipped != "" || strings.Join(fr.Abandoned, ",") != "a,b" {
		t.Fatalf("abandoned %v (skipped %q), want a,b", fr.Abandoned, fr.Skipped)
	}
	if len(fr.Shared) != 1 || fr.Shared[0].Kind != "indexes/unused_index" ||
		fr.Shared[0].Fingerprint != "fp1" || strings.Join(fr.Shared[0].Databases, ",") != "c,d" {
		t.Fatalf("shared anti-patterns %+v, want indexes/unused_index on fp1 for c,d", fr.Shared)
	}
	s := fr.Savings
	if s.Policy != PolicyRetireIdle || s.Members != 2 || s.Unpriced != 1 ||
		s.MonthlyUSD == nil || *s.MonthlyUSD != 12.5 {
		t.Fatalf("savings %+v, want retire_idle 2 members $12.5 and 1 unpriced", s)
	}
	if fr.SummarySource != ModeModel || fr.Summary != "Two databases look abandoned." ||
		len(m.purposes) != 1 || m.purposes[0] != PurposeFleet || fr.Usage.Calls != 1 {
		t.Fatalf("summary %q from %s, purposes %v", fr.Summary, fr.SummarySource, m.purposes)
	}
	if !strings.Contains(m.prompts[0], `<data label="fleet_review">`) {
		t.Fatal("the fleet facts were not wrapped as data")
	}
}

func TestReviewFleet_FleetOfOneSkips(t *testing.T) {
	m := constModel("x", nil)
	fr := ReviewFleet(context.Background(), m, fleetFixture()[:1], nil)
	if fr.Skipped == "" || m.calls() != 0 || len(fr.Abandoned) != 0 {
		t.Fatalf("fleet of 1: %+v after %d calls, want skipped", fr, m.calls())
	}
	if fr := ReviewFleet(context.Background(), m, nil, nil); fr.Skipped == "" {
		t.Fatal("an empty fleet was reviewed")
	}
}

func TestReviewFleet_ModelFailureOrNoModelUsesTheTemplate(t *testing.T) {
	for name, m := range map[string]Model{"failed": constModel("", errors.New("rate limited")),
		"none": nil, "empty": constModel("  ", nil)} {
		fr := ReviewFleet(context.Background(), m, fleetFixture(), nil)
		if fr.SummarySource != SourceTemplate || !strings.Contains(fr.Summary, "2 abandoned") ||
			!strings.Contains(fr.Summary, "$12.50") || fr.Reason == "" {
			t.Errorf("%s: summary %q from %s reason %q", name, fr.Summary, fr.SummarySource,
				fr.Reason)
		}
	}
}

func TestReviewFleet_NoPricesGivesNoDollarFigure(t *testing.T) {
	rs := fleetFixture()
	rs[0].MonthlyUSD = nil
	fr := ReviewFleet(context.Background(), nil, rs, nil)
	if fr.Savings.MonthlyUSD != nil || fr.Savings.Unpriced != 2 ||
		strings.Contains(fr.Summary, "$") {
		t.Fatalf("savings %+v summary %q, want no dollar figure", fr.Savings, fr.Summary)
	}
}
