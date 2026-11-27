package cases_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// A producer-written zero means the limit is already reached, not that
// urgency is unknown: the case stays unassessed with no number, yet it
// is marked so ranking puts it before every projection (cases-07 review).
// No concurrent access tests: the projection is a pure function.

func wantLimitReached(t *testing.T, c cases.Case, field string) {
	t.Helper()
	u := c.Urgency
	if !u.LimitReached || u.Assessed || u.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want limit reached, unassessed, no number", u)
	}
	want := "Urgency not assessed: " + field + " is zero, so the producer reports " +
		"the limit already reached"
	if c.WhyNow != want {
		t.Fatalf("why now = %q, want %q", c.WhyNow, want)
	}
	if strings.ContainsAny(c.WhyNow, "0123456789") {
		t.Fatalf("why now shows a number: %q", c.WhyNow)
	}
}

func TestUrgency_RunwayAtItsLimitIsLimitReached(t *testing.T) {
	tr := probes.RunwayTrend{Kind: probes.RunwayDiskUsed, Subject: probes.SubjectCluster,
		Samples: 48, FirstAt: urgencyAt.Add(-6 * time.Hour), LastAt: urgencyAt,
		LastValue: 1e11, Limit: 1e11, RatePerS: 50, R2: 0.97}
	f := onlyRunway(t, runway.EvalInput{Trends: []probes.RunwayTrend{tr}, TrendsOK: true})
	wantLimitReached(t, cases.ProjectFinding(asSource(t, f), nil), "seconds_to_limit")
}

func TestUrgency_OnlyAnExactZeroIsLimitReached(t *testing.T) {
	for name, v := range map[string]any{"negative": -1, "missing": nil, "string": "0",
		"tiny positive": 0.001} {
		d := map[string]any{"days_until_full": v, "r_squared": 0.9}
		if v == nil {
			delete(d, "days_until_full")
		}
		u := cases.UrgencyForFinding(storage(d))
		if u.LimitReached {
			t.Errorf("%s days_until_full gave limit reached: %+v", name, u)
		}
	}
	u := cases.UrgencyForFinding(storage(map[string]any{"days_until_full": 0.0,
		"r_squared": 0.1}))
	if !u.LimitReached {
		t.Fatalf("a full disk with a poor fit is still full: %+v", u)
	}
	u = cases.UrgencyForFinding(cases.SourceFinding{Category: "slow_query",
		Detail: map[string]any{"days_until_full": 0.0}})
	if u.LimitReached {
		t.Fatalf("an unknown category read a producer field: %+v", u)
	}
}
