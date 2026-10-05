package selfbudget

import (
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

func fullBudget() Budget {
	return Budget{CPUMsPerCycle: 600, DBTimeMsPerHour: 180_000, BlocksPerHour: 1_000_000,
		StorageBytes: 1 << 30}
}

func knownUsage() Usage {
	return Usage{CPUKnown: true, CPUMsPerCycle: 100, DBKnown: true,
		DBTimeMsPerHour: 1000, BlocksPerHour: 5000, StorageKnown: true, StorageBytes: 1 << 20}
}

func TestCheck_UnderBudgetHasNoBreach(t *testing.T) {
	if got := Check(fullBudget(), knownUsage()); len(got) != 0 {
		t.Fatalf("breaches = %+v, want none", got)
	}
}

func TestCheck_EachResourceOverBudget(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*Usage)
		res   Resource
		used  float64
		limit float64
		unit  string
	}{
		{"cpu", func(u *Usage) { u.CPUMsPerCycle = 601 }, ResourceCPU, 601, 600,
			"ms CPU per collector cycle"},
		{"db time", func(u *Usage) { u.DBTimeMsPerHour = 200_000 }, ResourceDBTime, 200_000,
			180_000, "ms database time per hour"},
		{"io", func(u *Usage) { u.BlocksPerHour = 2_000_000 }, ResourceIO, 2_000_000,
			1_000_000, "blocks per hour"},
		{"storage", func(u *Usage) { u.StorageBytes = 2 << 30 }, ResourceStorage, 2 << 30,
			1 << 30, "bytes"},
	}
	for _, tc := range cases {
		u := knownUsage()
		tc.mod(&u)
		got := Check(fullBudget(), u)
		if len(got) != 1 {
			t.Fatalf("%s: breaches = %+v, want exactly one", tc.name, got)
		}
		b := got[0]
		if b.Resource != tc.res || b.Used != tc.used || b.Limit != tc.limit || b.Unit != tc.unit {
			t.Errorf("%s: breach = %+v, want %s used %v limit %v unit %q", tc.name, b,
				tc.res, tc.used, tc.limit, tc.unit)
		}
		if b.Share() <= 1 {
			t.Errorf("%s: share = %v, want > 1", tc.name, b.Share())
		}
	}
}

func TestCheck_AllBreachesInFixedOrder(t *testing.T) {
	u := Usage{CPUKnown: true, CPUMsPerCycle: 1e6, DBKnown: true, DBTimeMsPerHour: 1e9,
		BlocksPerHour: 1e12, StorageKnown: true, StorageBytes: 1 << 40}
	got := Check(fullBudget(), u)
	want := []Resource{ResourceCPU, ResourceDBTime, ResourceIO, ResourceStorage}
	if len(got) != len(want) {
		t.Fatalf("breaches = %+v, want %v", got, want)
	}
	for i, r := range want {
		if got[i].Resource != r {
			t.Errorf("breach %d = %s, want %s", i, got[i].Resource, r)
		}
	}
}

// Boundaries: at the budget is within it; just above is a breach.
func TestCheck_ExactlyAtBudgetIsWithin(t *testing.T) {
	b := fullBudget()
	u := Usage{CPUKnown: true, CPUMsPerCycle: b.CPUMsPerCycle, DBKnown: true,
		DBTimeMsPerHour: b.DBTimeMsPerHour, BlocksPerHour: b.BlocksPerHour,
		StorageKnown: true, StorageBytes: b.StorageBytes}
	if got := Check(b, u); len(got) != 0 {
		t.Fatalf("at budget: breaches = %+v, want none", got)
	}
	u.CPUMsPerCycle = math.Nextafter(b.CPUMsPerCycle, math.Inf(1))
	u.StorageBytes = b.StorageBytes + 1
	if got := Check(b, u); len(got) != 2 {
		t.Fatalf("just above: breaches = %+v, want cpu and storage", got)
	}
}

func TestCheck_DisabledResourcesNeverBreach(t *testing.T) {
	u := Usage{CPUKnown: true, CPUMsPerCycle: 1e9, DBKnown: true, DBTimeMsPerHour: 1e9,
		BlocksPerHour: 1e9, StorageKnown: true, StorageBytes: 1 << 50}
	if got := Check(Budget{}, u); len(got) != 0 {
		t.Fatalf("zero budget: breaches = %+v, want none (0 disables)", got)
	}
	only := Budget{DBTimeMsPerHour: 1}
	got := Check(only, u)
	if len(got) != 1 || got[0].Resource != ResourceDBTime {
		t.Fatalf("db-time-only budget: breaches = %+v", got)
	}
}

// Unknown usage proves nothing: never a breach, whatever the number says.
func TestCheck_UnknownUsageNeverBreaches(t *testing.T) {
	u := Usage{CPUMsPerCycle: 1e9, DBTimeMsPerHour: 1e9, BlocksPerHour: 1e9,
		StorageBytes: 1 << 50}
	if got := Check(fullBudget(), u); len(got) != 0 {
		t.Fatalf("unknown usage: breaches = %+v, want none", got)
	}
	if got := Check(fullBudget(), Usage{}); len(got) != 0 {
		t.Fatalf("zero usage: breaches = %+v", got)
	}
}

func TestBudgetValidate(t *testing.T) {
	if err := fullBudget().Validate(); err != nil {
		t.Fatalf("valid budget: %v", err)
	}
	if err := (Budget{}).Validate(); err != nil {
		t.Fatalf("zero (disabled) budget: %v", err)
	}
	cases := map[string]Budget{
		"cpu_ms_per_cycle":    {CPUMsPerCycle: -1},
		"db_time_ms_per_hour": {DBTimeMsPerHour: math.NaN()},
		"blocks_per_hour":     {BlocksPerHour: math.Inf(1)},
		"storage":             {StorageBytes: -5},
	}
	for field, b := range cases {
		err := b.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want an error naming the field", field, err)
		}
	}
}

func TestBreachShareAndString(t *testing.T) {
	b := Breach{Resource: ResourceDBTime, Used: 300, Limit: 200, Unit: "ms database time per hour"}
	if b.Share() != 1.5 {
		t.Fatalf("share = %v, want 1.5", b.Share())
	}
	s := b.String()
	for _, want := range []string{"db_time", "300", "200", "ms database time per hour"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	if (Breach{Used: 5}).Share() != 0 {
		t.Fatal("share with a zero limit must be 0, not Inf")
	}
}

func TestUsageFromCost(t *testing.T) {
	c := selfcost.Cost{Known: true, DBTimeKnown: true, CycleSeconds: 60,
		DBTimeMsPerCycle: 100, BlocksPerCycle: 50, SchemaBytes: 4096}
	u := FromCost(c)
	if !u.DBKnown || u.DBTimeMsPerHour != 6000 || u.BlocksPerHour != 3000 {
		t.Fatalf("usage = %+v, want 6000 ms/h and 3000 blocks/h", u)
	}
	if !u.StorageKnown || u.StorageBytes != 4096 {
		t.Fatalf("storage = %+v", u)
	}
	if u.CPUKnown {
		t.Fatal("FromCost must not claim to know CPU")
	}
}

func TestUsageFromCost_UnknownWindows(t *testing.T) {
	cases := map[string]selfcost.Cost{
		"no window":       {DBTimeKnown: true, CycleSeconds: 60, DBTimeMsPerCycle: 9},
		"no statements":   {Known: true, CycleSeconds: 60, DBTimeMsPerCycle: 9},
		"zero cycle":      {Known: true, DBTimeKnown: true, DBTimeMsPerCycle: 9},
		"zero-value cost": {},
	}
	for name, c := range cases {
		u := FromCost(c)
		if u.DBKnown {
			t.Errorf("%s: DB time known = true from %+v", name, c)
		}
		if u.StorageKnown {
			t.Errorf("%s: storage known with SchemaBytes 0", name)
		}
	}
}
