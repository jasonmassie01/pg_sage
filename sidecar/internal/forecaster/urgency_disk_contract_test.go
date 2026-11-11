package forecaster

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
)

// The case urgency reads days_until_full, r_squared and covered_days
// exactly as forecast_disk_growth writes them (cases-07 with forecast-06:
// the retired storage_forecast was the storage growth producer before).
// No concurrent access tests: both sides are pure functions of the detail.

func diskCase(t *testing.T, f analyzer.Finding) cases.Case {
	t.Helper()
	raw, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return cases.ProjectFinding(cases.SourceFinding{Category: f.Category,
		Severity: cases.Severity(f.Severity), Detail: detail}, nil)
}

func TestUrgencyContract_DiskGrowthAgainstADeclaredCapacity(t *testing.T) {
	// 30 days ending at 70 GB, +1 GB/day, against 100 GB: 30 days to full.
	f := diskFindingWith(t, utcSizeDays(30, 70, 1),
		Capacity{Bytes: 100e9, Source: CapacityDeclared})
	c := diskCase(t, f)
	u := c.Urgency
	if !u.Assessed || u.TimeToImpactSeconds == nil ||
		math.Abs(*u.TimeToImpactSeconds-30*86400) > 1 {
		t.Fatalf("urgency = %+v, want 30 days", u)
	}
	if u.Basis != "days_until_full 30 from a 30-day regression, R² 1.00" {
		t.Fatalf("basis = %q", u.Basis)
	}
	if u.Inputs["capacity_source"] != CapacityDeclared || u.Inputs["capacity_bytes"] == nil {
		t.Fatalf("inputs lack the capacity it was judged against: %v", u.Inputs)
	}
	if !strings.HasPrefix(c.WhyNow, "About 30 days until storage is full (") {
		t.Fatalf("why now = %q", c.WhyNow)
	}
}

func TestUrgencyContract_DiskAlreadyFullIsLimitReached(t *testing.T) {
	f := diskFindingWith(t, utcSizeDays(30, 120, 1),
		Capacity{Bytes: 100e9, Source: CapacityDeclared})
	u := diskCase(t, f).Urgency
	if !u.LimitReached || u.Assessed || u.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want limit reached with no number", u)
	}
	if u.Inputs["days_until_full"] != 0.0 {
		t.Fatalf("inputs lack the zero the forecaster wrote: %v", u.Inputs)
	}
}

// Without a known capacity there is no time to full, so no deadline.
func TestUrgencyContract_DiskGrowthWithoutCapacityIsUnassessed(t *testing.T) {
	c := diskCase(t, diskFindingWith(t, utcSizeDays(30, 70, 1), Capacity{}))
	if c.Urgency.Assessed || c.Urgency.LimitReached {
		t.Fatalf("urgency = %+v, want unassessed", c.Urgency)
	}
	if !strings.HasPrefix(c.WhyNow, "Urgency not assessed: days_until_full") {
		t.Fatalf("why now = %q", c.WhyNow)
	}
}
