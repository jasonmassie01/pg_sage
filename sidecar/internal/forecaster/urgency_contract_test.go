package forecaster

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// The case urgency reads days_to_warn_pct and r_squared exactly as the
// connection saturation forecast writes them (cases-07).

func utcConnectionDays(n int, total func(int) float64) []DaySystemAgg {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	aggs := make([]DaySystemAgg, n)
	for i := range aggs {
		aggs[i] = DaySystemAgg{Day: base.AddDate(0, 0, i), MaxActiveBackends: 20,
			MaxTotalBackends: total(i), MaxConnections: 1000}
	}
	return aggs
}

func connectionCase(t *testing.T, aggs []DaySystemAgg) cases.Case {
	t.Helper()
	fs := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(fs) != 1 {
		t.Fatalf("forecastConnectionSaturation = %+v, want one finding", fs)
	}
	raw, err := json.Marshal(fs[0].Detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return cases.ProjectFinding(cases.SourceFinding{Category: fs[0].Category,
		Severity: cases.Severity(fs[0].Severity), Detail: detail}, nil)
}

func TestUrgencyContract_ConnectionSaturation(t *testing.T) {
	// 500 + 5/day for 30 days ends at 645; the 800 warning share is 31 days away.
	c := connectionCase(t, utcConnectionDays(30, func(i int) float64 {
		return 500 + float64(i)*5
	}))
	u := c.Urgency
	if !u.Assessed || u.TimeToImpactSeconds == nil ||
		math.Abs(*u.TimeToImpactSeconds-31*86400) > 1 {
		t.Fatalf("urgency = %+v, want 31 days", u)
	}
	if u.Basis != "days_to_warn_pct 31 from a daily regression, R² 1.00" {
		t.Fatalf("basis = %q", u.Basis)
	}
	if c.WhyNow != "About 31 days until connections reach the warning share of "+
		"max_connections (days_to_warn_pct 31 from a daily regression, R² 1.00)" {
		t.Fatalf("why now = %q", c.WhyNow)
	}
}

// Already at the warning share the forecaster reports 0 days: not a
// projection, so the case does not invent one.
func TestUrgencyContract_ConnectionAtThresholdIsUnassessed(t *testing.T) {
	c := connectionCase(t, utcConnectionDays(30, func(i int) float64 {
		return 600 + float64(i)*10
	}))
	if c.Urgency.Assessed || c.Urgency.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want unassessed", c.Urgency)
	}
}
