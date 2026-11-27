package forecaster

import (
	"math"
	"strings"
	"testing"
	"time"
)

// forecast-03: the checkpoint rule measures requested (WAL-triggered)
// checkpoints per hour between the last samples of closed days. Timed
// checkpoints are counted every checkpoint_timeout even when skipped, so
// an idle server at the default 5 min reads 288 a day and must not warn.

// No concurrent access tests: forecastCheckpointPressure and
// checkpointIntervals are pure functions over a slice the caller owns.

var ckptBase = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// ckptHistory is n closed days of split counters growing by timedPerDay
// and requestedPerDay, each read at the day's last sample (23:55 UTC).
func ckptHistory(n int, timedPerDay, requestedPerDay float64) []DaySystemAgg {
	aggs := make([]DaySystemAgg, n)
	for i := range aggs {
		day := ckptBase.AddDate(0, 0, i)
		aggs[i] = DaySystemAgg{
			Day:                  day,
			CheckpointsTimed:     5000 + timedPerDay*float64(i),
			CheckpointsRequested: 70 + requestedPerDay*float64(i),
			CheckpointsLastAt:    day.Add(23*time.Hour + 55*time.Minute),
		}
	}
	return aggs
}

func oneCheckpointFinding(t *testing.T, aggs []DaySystemAgg,
	cfg ForecasterConfig) map[string]any {
	t.Helper()
	f := forecastCheckpointPressure(aggs, cfg)
	if len(f) != 1 {
		t.Fatalf("want one checkpoint finding, got %+v", f)
	}
	if f[0].Category != "forecast_checkpoint_pressure" ||
		f[0].Severity != "warning" || f[0].ObjectType != "database" {
		t.Fatalf("finding = %+v", f[0])
	}
	return f[0].Detail
}

func hourlyRates(t *testing.T, detail map[string]any) []float64 {
	t.Helper()
	rates, ok := detail["hourly_rates"].([]float64)
	if !ok {
		t.Fatalf("hourly_rates missing or not []float64: %#v", detail)
	}
	return rates
}

func assertRates(t *testing.T, rates []float64, n int, want float64) {
	t.Helper()
	if len(rates) != n {
		t.Fatalf("got %d hourly rates %v, want %d", len(rates), rates, n)
	}
	for _, r := range rates {
		if math.Abs(r-want) > 1e-9 {
			t.Fatalf("rate %v, want %v on every interval: %v", r, want, rates)
		}
	}
}

func TestCheckpointPressure_IdleTimedOnlyGivesNoFinding(t *testing.T) {
	aggs := ckptHistory(10, 288, 0)
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("288 timed and 0 requested checkpoints a day warned: %+v", f)
	}
	if !checkpointEvaluable(aggs) {
		t.Fatal("10 days of split counters must evaluate the category")
	}
}

func TestCheckpointPressure_OneRequestedPerDayGivesNoFinding(t *testing.T) {
	// A nightly base backup: one requested checkpoint a day on top of
	// the timed ones crossed the old total-rate threshold of 12/hr.
	aggs := ckptHistory(10, 288, 1)
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("one requested checkpoint a day warned: %+v", f)
	}
}

func TestCheckpointPressure_TwoRequestedPerHourWarns(t *testing.T) {
	aggs := ckptHistory(10, 288, 48)
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	if got := d["ewma_requested_per_hour"].(float64); math.Abs(got-2) > 1e-9 {
		t.Fatalf("ewma_requested_per_hour = %v, want 2", got)
	}
	if got := d["threshold_per_hour"].(float64); got != 2 {
		t.Fatalf("threshold_per_hour = %v, want the default 2", got)
	}
	if got := d["requested_share"].(float64); math.Abs(got-48.0/336) > 1e-9 {
		t.Fatalf("requested_share = %v, want 48/336", got)
	}
	if got := d["checkpoints_requested"].(float64); got != 9*48 {
		t.Fatalf("checkpoints_requested = %v, want 432 over 9 intervals", got)
	}
	if got := d["checkpoints_timed"].(float64); got != 9*288 {
		t.Fatalf("checkpoints_timed = %v, want 2592", got)
	}
	assertRates(t, hourlyRates(t, d), 9, 2)
}

func TestCheckpointPressure_JustUnderThresholdGivesNoFinding(t *testing.T) {
	aggs := ckptHistory(10, 288, 1.99*24)
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("1.99 requested checkpoints per hour warned: %+v", f)
	}
}

func TestCheckpointPressure_RecommendationNamesMaxWalSize(t *testing.T) {
	f := forecastCheckpointPressure(ckptHistory(10, 288, 120), ForecasterConfig{})
	if len(f) != 1 {
		t.Fatalf("5 requested per hour gave %+v", f)
	}
	rec := f[0].Recommendation
	for _, want := range []string{"1080 requested", "max_wal_size", "advisor.enabled",
		"wal_tuning"} {
		if !strings.Contains(rec, want) {
			t.Errorf("recommendation %q does not mention %q", rec, want)
		}
	}
	if strings.Contains(rec, "checkpoint_completion_target") {
		t.Errorf("recommendation %q suggests checkpoint_completion_target, "+
			"which does not change checkpoint frequency", rec)
	}
	if !strings.Contains(f[0].Title, "5.0") {
		t.Errorf("title %q does not carry the requested rate", f[0].Title)
	}
}

func TestCheckpointPressure_ConfiguredThreshold(t *testing.T) {
	aggs := ckptHistory(10, 288, 72) // 3 requested per hour
	cfg := ForecasterConfig{CheckpointRequestedPerHour: 5}
	if f := forecastCheckpointPressure(aggs, cfg); len(f) != 0 {
		t.Fatalf("3/hr under a 5/hr threshold warned: %+v", f)
	}
	cfg.CheckpointRequestedPerHour = 2.5
	d := oneCheckpointFinding(t, aggs, cfg)
	if got := d["threshold_per_hour"].(float64); got != 2.5 {
		t.Fatalf("threshold_per_hour = %v, want the configured 2.5", got)
	}
}

func TestCheckpointPressure_ZeroOrNegativeThresholdUsesDefault(t *testing.T) {
	for _, limit := range []float64{0, -1, math.NaN()} {
		aggs := ckptHistory(10, 288, 1.5*24)
		cfg := ForecasterConfig{CheckpointRequestedPerHour: limit}
		if f := forecastCheckpointPressure(aggs, cfg); len(f) != 0 {
			t.Fatalf("threshold %v: 1.5/hr warned, so the limit was not the "+
				"default 2: %+v", limit, f)
		}
		d := oneCheckpointFinding(t, ckptHistory(10, 288, 48), cfg)
		if got := d["threshold_per_hour"].(float64); got != 2 {
			t.Fatalf("threshold %v: threshold_per_hour = %v, want 2", limit, got)
		}
	}
}

func TestCheckpointPressure_CounterDropSkipsTheDay(t *testing.T) {
	steady := oneCheckpointFinding(t, ckptHistory(10, 288, 72), ForecasterConfig{})
	aggs := ckptHistory(10, 288, 72)
	for i := 5; i < len(aggs); i++ { // pg_stat_reset_shared during day 5
		aggs[i].CheckpointsTimed = 288 * float64(i-5)
		aggs[i].CheckpointsRequested = 72 * float64(i-5)
	}
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	assertRates(t, hourlyRates(t, d), 8, 3)
	if d["ewma_requested_per_hour"] != steady["ewma_requested_per_hour"] {
		t.Fatalf("ewma %v with the reset day, %v without: the drop was not skipped",
			d["ewma_requested_per_hour"], steady["ewma_requested_per_hour"])
	}
}

func TestCheckpointPressure_TimedDropAloneSkipsTheDay(t *testing.T) {
	aggs := ckptHistory(10, 288, 72)
	aggs[6].CheckpointsTimed = 1
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	// The timed counter drops into day 6 while requested grows: the
	// counters reset together, so the interval 5->6 is not trusted.
	assertRates(t, hourlyRates(t, d), 8, 3)
}

func TestCheckpointPressure_MissingDayKeepsTheHourlyRate(t *testing.T) {
	aggs := ckptHistory(11, 288, 72)
	gap := append(append([]DaySystemAgg(nil), aggs[:5]...), aggs[6:]...)
	d := oneCheckpointFinding(t, gap, ForecasterConfig{})
	assertRates(t, hourlyRates(t, d), 9, 3)
}

func TestCheckpointPressure_DayWithoutSplitIsSkipped(t *testing.T) {
	aggs := ckptHistory(11, 288, 72)
	aggs[5] = DaySystemAgg{Day: aggs[5].Day, CheckpointsUnknown: true}
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	assertRates(t, hourlyRates(t, d), 9, 3)
}

func TestCheckpointPressure_RateUsesHoursBetweenLastSamples(t *testing.T) {
	// The collector stopped at noon on day 4: its counters are 12 hours
	// short, and so is the interval. Both rates stay 3/hr; dividing by
	// 24 hours would read 1.5 and 4.5.
	aggs := ckptHistory(10, 288, 72)
	aggs[4].CheckpointsLastAt = aggs[4].Day.Add(11*time.Hour + 55*time.Minute)
	aggs[4].CheckpointsRequested -= 36
	aggs[4].CheckpointsTimed -= 144
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	assertRates(t, hourlyRates(t, d), 9, 3)
}

func TestCheckpointPressure_NonIncreasingSampleTimeIsSkipped(t *testing.T) {
	aggs := ckptHistory(10, 288, 72)
	aggs[6].CheckpointsLastAt = aggs[5].CheckpointsLastAt
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	rates := hourlyRates(t, d)
	if len(rates) != 8 {
		t.Fatalf("a zero-hour interval was kept: %v", rates)
	}
	for _, r := range rates {
		if math.IsInf(r, 0) || math.IsNaN(r) {
			t.Fatalf("non-finite rate %v in %v", r, rates)
		}
	}
}

func TestCheckpointPressure_KnownDayWithoutSampleTimeIsSkipped(t *testing.T) {
	aggs := ckptHistory(10, 288, 72)
	aggs[3].CheckpointsLastAt = time.Time{}
	d := oneCheckpointFinding(t, aggs, ForecasterConfig{})
	if rates := hourlyRates(t, d); len(rates) != 8 {
		t.Fatalf("a day without a sample time was used: %v", rates)
	}
}

func TestCheckpointPressure_LegacyRowsAreNotEvaluated(t *testing.T) {
	aggs := makeSysAggs(10, func(int) DaySystemAgg {
		return DaySystemAgg{CheckpointsUnknown: true, MaxConnections: 100}
	})
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("legacy rows gave %+v", f)
	}
	cats := evaluatedSystemCategories(aggs)
	if hasCategory(cats, "forecast_checkpoint_pressure") {
		t.Fatalf("legacy rows evaluate the checkpoint category: %v", cats)
	}
	for _, c := range []string{"forecast_connection_saturation",
		"forecast_cache_pressure"} {
		if !hasCategory(cats, c) {
			t.Fatalf("%s must stay evaluated: %v", c, cats)
		}
	}
}

func TestCheckpointPressure_NoHistoryIsNotEvaluated(t *testing.T) {
	for _, aggs := range [][]DaySystemAgg{nil, {}} {
		if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
			t.Fatalf("no history gave %+v", f)
		}
		if hasCategory(evaluatedSystemCategories(aggs), "forecast_checkpoint_pressure") {
			t.Fatal("no history evaluates the checkpoint category")
		}
	}
}

func TestCheckpointPressure_EvaluatedNeedsMinDataPointsRates(t *testing.T) {
	// 8 days give 7 rates (minDataPoints) and evaluate; 7 days give 6.
	if !checkpointEvaluable(ckptHistory(minDataPoints+1, 288, 0)) {
		t.Fatal("minDataPoints rates must evaluate the category")
	}
	if !hasCategory(evaluatedSystemCategories(ckptHistory(minDataPoints+1, 288, 0)),
		"forecast_checkpoint_pressure") {
		t.Fatal("evaluatedSystemCategories drops an evaluable checkpoint category")
	}
	short := ckptHistory(minDataPoints, 288, 120)
	if checkpointEvaluable(short) {
		t.Fatal("minDataPoints-1 rates must not evaluate the category")
	}
	if f := forecastCheckpointPressure(short, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("too few rates warned: %+v", f)
	}
}

func TestCheckpointPressure_ResetLeavingTooFewRatesIsNotEvaluated(t *testing.T) {
	aggs := ckptHistory(minDataPoints+1, 288, 120)
	aggs[3].CheckpointsRequested = 0
	aggs[3].CheckpointsTimed = 0
	if checkpointEvaluable(aggs) {
		t.Fatal("a drop left 6 rates, which must not evaluate the category")
	}
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("too few rates after a reset warned: %+v", f)
	}
}

func TestCheckpointPressure_NoCheckpointsGivesZeroShare(t *testing.T) {
	// A frozen counter (no checkpoint at all) is a rate of 0, not a
	// division by zero in the share.
	aggs := ckptHistory(10, 0, 0)
	if f := forecastCheckpointPressure(aggs, ForecasterConfig{}); len(f) != 0 {
		t.Fatalf("no checkpoints warned: %+v", f)
	}
	if !checkpointEvaluable(aggs) {
		t.Fatal("frozen counters are known and must evaluate the category")
	}
}
