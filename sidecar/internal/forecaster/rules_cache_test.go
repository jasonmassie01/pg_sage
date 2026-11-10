package forecaster

import (
	"math"
	"strings"
	"testing"
	"time"
)

// forecast-04: the cache forecast reads each closed day's interval hit
// ratio (the blks_hit and blks_read growth collection-06 records per
// snapshot), not the lifetime ratio. A day without enough block accesses
// is unknown and left out, never encoded as -1; "low" judges the newest
// usable day; "declining" needs a drop of at least 1 percentage point.

// No concurrent access tests: the cache rule and the day gate are pure
// functions over a slice the caller owns.

const cacheTestBlocks = 1e6

// cache95 is the default threshold.
var cache95 = ForecasterConfig{CacheWarnThreshold: 0.95}

// cacheDays builds one closed day per ratio, oldest first, ending
// yesterday. NaN is an unknown day (no interval data).
func cacheDays(ratios ...float64) []DaySystemAgg {
	first := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -len(ratios))
	aggs := make([]DaySystemAgg, len(ratios))
	for i, r := range ratios {
		aggs[i] = DaySystemAgg{Day: first.AddDate(0, 0, i),
			CacheHitRatio: r, CacheBlocksAccessed: cacheTestBlocks}
		if math.IsNaN(r) {
			aggs[i] = DaySystemAgg{Day: first.AddDate(0, 0, i), CacheUnknown: true}
		}
	}
	return aggs
}

// linearCache is n days falling linearly from start to end.
func linearCache(n int, start, end float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + (end-start)*float64(i)/float64(n-1)
	}
	return out
}

func cacheFindingOf(t *testing.T, aggs []DaySystemAgg, threshold float64,
	kind string) map[string]any {
	t.Helper()
	for _, f := range forecastCachePressure(aggs, ForecasterConfig{
		CacheWarnThreshold: threshold}) {
		if f.Category != "forecast_cache_pressure" {
			t.Fatalf("category = %q", f.Category)
		}
		if f.Detail["forecast_type"] == kind {
			return f.Detail
		}
	}
	return nil
}

func TestCachePressure_UnknownDayIsExcluded(t *testing.T) {
	// Seven days, one unknown: six usable days, so nothing is judged.
	week := cacheDays(0.80, 0.80, 0.80, math.NaN(), 0.80, 0.80, 0.80)
	if got := forecastCachePressure(week, ForecasterConfig{CacheWarnThreshold: 0.95}); got != nil {
		t.Fatalf("six usable days gave findings: %+v", got)
	}
	if hasCategory(evaluatedSystemCategories(week), "forecast_cache_pressure") {
		t.Fatal("six usable days evaluate the cache category")
	}
	// The newest day is unknown and reads 0 if it leaked in: it must not
	// become the judged day, nor a -100% point.
	eight := cacheDays(0.99, 0.99, 0.99, 0.99, 0.99, 0.99, 0.99, math.NaN())
	eight[7].CacheHitRatio = 0
	if got := forecastCachePressure(eight, cache95); len(got) != 0 {
		t.Fatalf("an unknown newest day was judged: %+v", got)
	}
	if n := len(knownCacheDays(eight)); n != 7 {
		t.Fatalf("known days = %d, want 7 (the unknown day excluded)", n)
	}
}

func TestCachePressure_SixUsableDaysNotEvaluated(t *testing.T) {
	six := cacheDays(0.5, 0.5, 0.5, 0.5, 0.5, 0.5)
	if got := forecastCachePressure(six, ForecasterConfig{CacheWarnThreshold: 0.95}); got != nil {
		t.Fatalf("six days gave findings: %+v", got)
	}
	if hasCategory(evaluatedSystemCategories(six), "forecast_cache_pressure") {
		t.Fatal("six usable days evaluate the cache category")
	}
	seven := cacheDays(0.5, 0.5, 0.5, 0.5, 0.5, 0.5, 0.5)
	if !hasCategory(evaluatedSystemCategories(seven), "forecast_cache_pressure") {
		t.Fatal("seven usable days do not evaluate the cache category")
	}
	if cacheFindingOf(t, seven, 0.95, "cache_pressure") == nil {
		t.Fatal("seven days at 50% gave no low finding")
	}
	if got := evaluatedSystemCategories(nil); hasCategory(got, "forecast_cache_pressure") {
		t.Fatalf("no history evaluates the cache category: %v", got)
	}
}

func TestCachePressure_DriftBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end float64
		want       bool
	}{
		{"0.3 pp drift of a lifetime-like ratio", 0.9991, 0.9961, false},
		{"0.99 pp drop", 0.99, 0.9801, false},
		{"exactly 1.0 pp drop", 0.99, 0.98, true},
		{"2 pp drop", 0.99, 0.97, true},
		{"2 pp rise", 0.97, 0.99, false},
	} {
		detail := cacheFindingOf(t, cacheDays(linearCache(7, tc.start, tc.end)...), 0.95,
			"cache_decline")
		if (detail != nil) != tc.want {
			t.Errorf("%s: declining finding = %v, want %v", tc.name, detail != nil, tc.want)
			continue
		}
		if detail == nil {
			continue
		}
		wantDrop := (tc.start - tc.end) * 100
		if got := detail["drop_pp"].(float64); math.Abs(got-wantDrop) > 1e-6 {
			t.Errorf("%s: drop_pp = %v, want %v", tc.name, got, wantDrop)
		}
		if r2 := detail["r_squared"].(float64); r2 < 0.99 {
			t.Errorf("%s: r_squared = %v, want about 1 on a linear series", tc.name, r2)
		}
		if got := detail["day_hit_ratio"].(float64); math.Abs(got-tc.end) > 1e-9 {
			t.Errorf("%s: day_hit_ratio = %v, want the newest day %v", tc.name, got, tc.end)
		}
	}
}

func TestCachePressure_DeclineNeedsAFit(t *testing.T) {
	ratios := []float64{0.99, 0.95, 0.99, 0.95, 0.98, 0.95, 0.975}
	aggs := cacheDays(ratios...)
	reg := LinearRegression(sysToDataPoints(aggs, func(a DaySystemAgg) float64 {
		return a.CacheHitRatio
	}))
	if reg.R2 > 0.5 || -reg.Slope*6 < 0.01 {
		t.Fatalf("fixture must drop >= 1 pp with R2 <= 0.5, got slope %v R2 %v",
			reg.Slope, reg.R2)
	}
	if d := cacheFindingOf(t, aggs, 0.90, "cache_decline"); d != nil {
		t.Fatalf("a noisy series (R2 %.2f) gave a declining finding: %+v", reg.R2, d)
	}
}

func TestCachePressure_LowJudgesTheNewestDay(t *testing.T) {
	aggs := cacheDays(0.99, 0.99, 0.99, 0.99, 0.99, 0.99, 0.80)
	got := forecastCachePressure(aggs, ForecasterConfig{CacheWarnThreshold: 0.95})
	var low *map[string]any
	for i := range got {
		if got[i].Detail["forecast_type"] == "cache_pressure" {
			low = &got[i].Detail
			if !strings.Contains(got[i].Title, "80.0%") {
				t.Errorf("title = %q, want the newest day's 80.0%%", got[i].Title)
			}
			if got[i].Severity != "warning" || got[i].ObjectType != "database" {
				t.Errorf("finding = %+v", got[i])
			}
		}
	}
	if low == nil {
		t.Fatalf("newest day at 80%% gave no low finding: %+v", got)
	}
	d := *low
	if r := d["day_hit_ratio"].(float64); r != 0.80 {
		t.Errorf("day_hit_ratio = %v, want 0.80", r)
	}
	if e := d["ewma_value"].(float64); !(e > 0.80 && e < 0.99) {
		t.Errorf("ewma_value = %v, want between the newest day and the history", e)
	}
	if d["threshold"].(float64) != 0.95 || d["days_evaluated"].(int) != 7 {
		t.Errorf("detail = %+v", d)
	}
	if day, _ := d["day"].(time.Time); !day.Equal(aggs[6].Day) {
		t.Errorf("day = %v, want the newest day %s", d["day"], aggs[6].Day)
	}
	if d["blocks_accessed"].(float64) != cacheTestBlocks {
		t.Errorf("blocks_accessed = %v", d["blocks_accessed"])
	}
	// A recovered newest day is not low, however low the history was.
	recovered := cacheDays(0.80, 0.80, 0.80, 0.80, 0.80, 0.80, 0.99)
	if d := cacheFindingOf(t, recovered, 0.95, "cache_pressure"); d != nil {
		t.Fatalf("a recovered newest day (99%%) was judged low: %+v", d)
	}
}

func TestCachePressure_InvalidRatios(t *testing.T) {
	// A legacy percent is normalised: 99.1 is 99.1%, not 9910%.
	percent := cacheDays(99.1, 99.1, 99.1, 99.1, 99.1, 99.1, 80)
	d := cacheFindingOf(t, percent, 0.95, "cache_pressure")
	if d == nil || math.Abs(d["day_hit_ratio"].(float64)-0.80) > 1e-9 {
		t.Fatalf("percent newest day 80 = %+v, want a low finding at 0.80", d)
	}
	if got := forecastCachePressure(cacheDays(99.1, 99.1, 99.1, 99.1, 99.1, 99.1, 99.1),
		ForecasterConfig{CacheWarnThreshold: 0.95}); len(got) != 0 {
		t.Fatalf("a steady 99.1%% gave findings: %+v", got)
	}
	// A negative, above-100%, infinite or NaN value never reaches the EWMA
	// or the regression, even on a day not marked unknown.
	for _, bad := range []float64{-1, -0.5, 150, math.Inf(1)} {
		aggs := cacheDays(0.99, 0.99, 0.99, 0.99, 0.99, 0.99, 0.99, 0.99)
		aggs[7].CacheHitRatio = bad
		if got := forecastCachePressure(aggs, cache95); len(got) != 0 {
			t.Errorf("ratio %v reached the rule: %+v", bad, got)
		}
		if n := len(knownCacheDays(aggs)); n != 7 {
			t.Errorf("ratio %v: known days = %d, want 7", bad, n)
		}
	}
	aggs := cacheDays(0.99, 0.99, 0.99, 0.99, 0.99, 0.99, 0.99)
	aggs[3].CacheHitRatio = math.NaN()
	if hasCategory(evaluatedSystemCategories(aggs), "forecast_cache_pressure") {
		t.Error("a NaN day counted as usable")
	}
}

func TestGateCacheDays_MinBlocksBoundary(t *testing.T) {
	aggs := cacheDays(0.99, 0.99, 0.99)
	aggs[0].CacheBlocksAccessed = 99_999
	aggs[1].CacheBlocksAccessed = 100_000
	aggs[2].CacheBlocksAccessed = 0
	gateCacheDays(aggs, 100_000)
	if !aggs[0].CacheUnknown || aggs[1].CacheUnknown || !aggs[2].CacheUnknown {
		t.Fatalf("gate at 100000: unknown = %v %v %v, want true false true",
			aggs[0].CacheUnknown, aggs[1].CacheUnknown, aggs[2].CacheUnknown)
	}
	// A gate never revives a day the read already marked unknown.
	dead := []DaySystemAgg{{CacheUnknown: true, CacheBlocksAccessed: 1e9}}
	gateCacheDays(dead, 1)
	if !dead[0].CacheUnknown {
		t.Fatal("the gate revived an unknown day")
	}
	gateCacheDays(nil, 100_000) // nil history: nothing to gate, no panic
}

func TestCacheMinBlocks_Default(t *testing.T) {
	for _, tc := range []struct {
		in, want int64
	}{{0, 100_000}, {-5, 100_000}, {1, 1}, {250_000, 250_000}} {
		if got := cacheMinBlocks(ForecasterConfig{CacheMinBlocksPerDay: tc.in}); got != tc.want {
			t.Errorf("cacheMinBlocks(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A stats reset mid-window: the read leaves the reset day without
// interval data (collection-06 gives a nil interval), so it is unknown;
// the days either side keep their ratios and nothing goes negative.
func TestCachePressure_ResetDayIsSkipped(t *testing.T) {
	aggs := cacheDays(0.99, 0.99, 0.99, math.NaN(), 0.99, 0.99, 0.99, 0.99)
	known := knownCacheDays(aggs)
	if len(known) != 7 {
		t.Fatalf("known days = %d, want 7", len(known))
	}
	for _, a := range known {
		if a.CacheHitRatio < 0 || a.Day.Equal(aggs[3].Day) {
			t.Fatalf("reset day or a negative ratio kept: %+v", a)
		}
	}
	if got := forecastCachePressure(aggs, cache95); len(got) != 0 {
		t.Fatalf("a reset day produced findings: %+v", got)
	}
}
