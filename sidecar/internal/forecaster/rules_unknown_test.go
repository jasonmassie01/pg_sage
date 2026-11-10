package forecaster

import (
	"math"
	"testing"
)

// forecast-07: a day whose system inputs are unknown (every snapshot of the
// day wrote JSON null) must be skipped by the rules that use that input,
// not read as zero and not break the other rules.
// No concurrent access tests: the rules are pure functions of their inputs.

// sizeDays builds n days growing gbPerDay from 100 GB, with the listed day
// offsets marked as unknown size.
func sizeDays(n int, gbPerDay float64, unknown ...int) []DaySystemAgg {
	skip := map[int]bool{}
	for _, u := range unknown {
		skip[u] = true
	}
	return makeSysAggs(n, func(i int) DaySystemAgg {
		if skip[i] {
			return DaySystemAgg{DBSizeUnknown: true}
		}
		size := 100e9 + float64(i)*gbPerDay*1e9
		return DaySystemAgg{AvgDBSizeBytes: size, MaxDBSizeBytes: size}
	})
}

func hasCategory(cats []string, want string) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}

func diskDetail(t *testing.T, aggs []DaySystemAgg) map[string]any {
	t.Helper()
	findings := forecastDiskGrowth(aggs, ForecasterConfig{DiskWarnGrowthGBDay: 5})
	if len(findings) != 1 {
		t.Fatalf("want one disk finding, got %d: %+v", len(findings), findings)
	}
	return findings[0].Detail
}

func TestDiskGrowth_SevenKnownOfEightOpensFinding(t *testing.T) {
	d := diskDetail(t, sizeDays(8, 6, 3))
	rate, ok := d["growth_rate_gb_day"].(float64)
	if !ok || math.Abs(rate-6) > 0.05 {
		t.Fatalf("growth_rate_gb_day = %v, want about 6 (the null day read as 0)", d)
	}
	if cur := d["current_size_gb"].(float64); math.Abs(cur-142) > 0.01 {
		t.Fatalf("current_size_gb = %v, want 142", cur)
	}
	if r2 := d["r_squared"].(float64); r2 < 0.999 {
		t.Fatalf("r_squared = %v: the unknown day leaked into the fit", r2)
	}
}

func TestDiskGrowth_SixKnownOfEightNoFindingNotEvaluated(t *testing.T) {
	aggs := sizeDays(8, 6, 2, 5)
	if f := forecastDiskGrowth(aggs, ForecasterConfig{DiskWarnGrowthGBDay: 5}); len(f) != 0 {
		t.Fatalf("6 known days must not open a disk finding: %+v", f)
	}
	cats := evaluatedSystemCategories(aggs)
	if hasCategory(cats, "forecast_disk_growth") {
		t.Fatalf("disk must stay unevaluated with 6 known-size days: %v", cats)
	}
	for _, c := range []string{"forecast_connection_saturation",
		"forecast_cache_pressure"} {
		if !hasCategory(cats, c) {
			t.Fatalf("%s must stay evaluated: %v", c, cats)
		}
	}
	// forecast-03: the fixture carries no split checkpoint counters.
	if hasCategory(cats, "forecast_checkpoint_pressure") {
		t.Fatalf("checkpoint evaluated without checkpoint counters: %v", cats)
	}
}

func TestDiskGrowth_NullLatestDayUsesPreviousSize(t *testing.T) {
	d := diskDetail(t, sizeDays(9, 6, 8))
	if cur := d["current_size_gb"].(float64); math.Abs(cur-142) > 0.01 {
		t.Fatalf("current_size_gb = %v, want 142 from the last known day", cur)
	}
}

func TestDiskGrowth_AllDaysNullNoFinding(t *testing.T) {
	aggs := sizeDays(10, 6, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	if f := forecastDiskGrowth(aggs, ForecasterConfig{DiskWarnGrowthGBDay: 5}); len(f) != 0 {
		t.Fatalf("all-unknown sizes must give no disk finding: %+v", f)
	}
	// forecast-03: without split counters the checkpoint rule is not
	// evaluated either, leaving connections and cache.
	if cats := evaluatedSystemCategories(aggs); hasCategory(cats, "forecast_disk_growth") ||
		len(cats) != 2 {
		t.Fatalf("all-unknown sizes over 10 days: evaluated = %v, want connections and cache",
			cats)
	}
}

func TestEvaluatedSystemCategories_Boundaries(t *testing.T) {
	cases := []struct {
		name     string
		aggs     []DaySystemAgg
		wantDisk bool
	}{
		{"nil history", nil, true},
		{"six days all unknown: fresh install", sizeDays(6, 6, 0, 1, 2, 3, 4, 5), true},
		{"seven days all known", sizeDays(7, 6), true},
		{"seven days one unknown", sizeDays(7, 6, 0), false},
		{"eight days one unknown", sizeDays(8, 6, 7), true},
	}
	for _, tc := range cases {
		cats := evaluatedSystemCategories(tc.aggs)
		if got := hasCategory(cats, "forecast_disk_growth"); got != tc.wantDisk {
			t.Errorf("%s: disk evaluated = %v, want %v (%v)", tc.name, got, tc.wantDisk, cats)
		}
		// forecast-03: these fixtures carry no split checkpoint counters,
		// so the checkpoint category is never among them.
		if hasCategory(cats, "forecast_checkpoint_pressure") {
			t.Errorf("%s: checkpoint evaluated without counters: %v", tc.name, cats)
		}
		// forecast-04: the cache category needs minDataPoints usable days;
		// these fixtures' zero-value days count, so it follows the length.
		if got := hasCategory(cats, "forecast_cache_pressure"); got !=
			(len(tc.aggs) >= minDataPoints) {
			t.Errorf("%s: cache evaluated = %v with %d days", tc.name, got, len(tc.aggs))
		}
		want := 1
		if tc.wantDisk {
			want++
		}
		if len(tc.aggs) >= minDataPoints {
			want++
		}
		if len(cats) != want {
			t.Errorf("%s: %d categories, want %d: %v", tc.name, len(cats), want, cats)
		}
	}
}

func connDays(n int, unknownMax ...int) []DaySystemAgg {
	skip := map[int]bool{}
	for _, u := range unknownMax {
		skip[u] = true
	}
	return makeSysAggs(n, func(i int) DaySystemAgg {
		a := DaySystemAgg{MaxTotalBackends: float64(50 + i*3), MaxConnections: 200}
		if skip[i] {
			a.MaxConnections, a.MaxConnectionsUnknown = 0, true
		}
		return a
	})
}

func TestConnectionSaturation_NullLatestMaxConnectionsSkipped(t *testing.T) {
	findings := forecastConnectionSaturation(connDays(30, 29),
		ForecasterConfig{ConnectionWarnPct: 80})
	if len(findings) != 1 {
		t.Fatalf("a null max_connections day must be skipped, not end the rule: %+v", findings)
	}
	d := findings[0].Detail
	if d["max_connections"].(float64) != 200 {
		t.Fatalf("max_connections = %v, want 200 from the last known day", d["max_connections"])
	}
	if d["current_in_use"].(float64) != float64(50+28*3) {
		t.Fatalf("current_in_use = %v, want %d", d["current_in_use"], 50+28*3)
	}
}

func TestConnectionSaturation_AllMaxConnectionsNull(t *testing.T) {
	all := make([]int, 30)
	for i := range all {
		all[i] = i
	}
	if f := forecastConnectionSaturation(connDays(30, all...),
		ForecasterConfig{ConnectionWarnPct: 80}); len(f) != 0 {
		t.Fatalf("no known max_connections must give no finding: %+v", f)
	}
}

func TestConnectionSaturation_UnknownBackendsDaySkipped(t *testing.T) {
	aggs := connDays(30)
	// A day with no backend counts would read as 0 and drag the slope.
	aggs[29] = DaySystemAgg{Day: aggs[29].Day, MaxConnections: 200, BackendsUnknown: true}
	findings := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(findings) != 1 {
		t.Fatalf("want one finding, got %+v", findings)
	}
	if cur := findings[0].Detail["current_in_use"].(float64); cur != float64(50+28*3) {
		t.Fatalf("current_in_use = %v: the unknown day was read as 0", cur)
	}
}

// forecast-03: these forecast-07 cases read the split requested counter;
// a day without it is skipped, never read as 0.
func TestCheckpointPressure_UnknownDayIsNotACounterReset(t *testing.T) {
	// 24 requested checkpoints/day (1/hr) on a counter already at 1e6.
	// Read as 0, the unknown day turns the next delta into a 41,000/hr spike.
	aggs := ckptHistory(10, 288, 24)
	for i := range aggs {
		aggs[i].CheckpointsRequested += 1e6
	}
	aggs[8] = DaySystemAgg{Day: aggs[8].Day, CheckpointsUnknown: true}
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("an unknown checkpoint day must not read as a reset: %+v", f)
	}
}

func TestCheckpointPressure_RateAcrossGapUsesElapsedHours(t *testing.T) {
	// 480 requested checkpoints/day = 20/hr, over the 2/hr threshold.
	aggs := ckptHistory(11, 288, 480)
	aggs[5] = DaySystemAgg{Day: aggs[5].Day, CheckpointsUnknown: true}
	f := forecastCheckpointPressure(aggs, ForecasterConfig{})
	if len(f) != 1 {
		t.Fatalf("want one checkpoint finding, got %+v", f)
	}
	rates := f[0].Detail["hourly_rates"].([]float64)
	if len(rates) != 9 {
		t.Fatalf("want 9 rates between 10 known days, got %v", rates)
	}
	for _, r := range rates {
		if math.Abs(r-20) > 1e-9 {
			t.Fatalf("rate %v, want 20/hr on every interval including the gap: %v", r, rates)
		}
	}
}

func TestCheckpointPressure_TooFewKnownDays(t *testing.T) {
	aggs := ckptHistory(8, 288, 480)
	aggs[3].CheckpointsUnknown = true
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("7 known checkpoint days (< minDataPoints+1) must not fire: %+v", f)
	}
}
