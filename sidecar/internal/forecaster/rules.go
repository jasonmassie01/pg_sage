package forecaster

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

const minDataPoints = 7

// forecastDiskGrowth warns when database size is growing faster
// than the configured threshold in GB/day. Only days with a measured
// size count, and minDataPoints of them are needed (forecast-07); the
// trend must pass trendGate (forecast-08).
func forecastDiskGrowth(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	known := knownSizeDays(aggs)
	if len(known) < minDataPoints {
		return nil
	}

	points := sysToDataPoints(known, func(a DaySystemAgg) float64 {
		return a.MaxDBSizeBytes
	})
	reg := LinearRegression(points)
	gate := trendGate(known, reg, cfg)

	gbPerDay := reg.Slope / 1e9
	if gbPerDay <= cfg.DiskWarnGrowthGBDay || !gate.ok {
		return nil
	}

	current := known[len(known)-1].MaxDBSizeBytes
	currentGB := current / 1e9
	return []analyzer.Finding{{
		Category:   "forecast_disk_growth",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Database growing at %.1f GB/day", gbPerDay,
		),
		Detail: map[string]any{
			"forecast_type":      "disk_growth",
			"current_size_gb":    currentGB,
			"growth_rate_gb_day": gbPerDay,
			"r_squared":          reg.R2,
			"span_days":          gate.spanDays,
			"covered_days":       gate.coveredDays,
			// Bytes and the newest day for the capacity fields
			// (withDiskCapacity).
			"current_size_bytes":    current,
			"growth_rate_bytes_day": reg.Slope,
			"as_of_day":             known[len(known)-1].Day,
		},
		Recommendation: fmt.Sprintf(
			"Disk is growing at %.1f GB/day. "+
				"Review table bloat, TOAST usage, and "+
				"retention policies.", gbPerDay,
		),
	}}
}

// forecastQueryVolume flags rapid week-over-week query growth: the 7
// closed days ending yesterday against the 7 before, all of them covered
// calendar days (closedQueryDays, forecast-08).
func forecastQueryVolume(
	qAggs []DayQueryAgg, _ ForecasterConfig,
) []analyzer.Finding {
	w, ok := weekOverWeek(qAggs)
	if !ok {
		return nil
	}
	var severity string
	switch {
	case w.pct > 100:
		severity = "critical"
	case w.pct > 50:
		severity = "warning"
	default:
		return nil
	}
	return []analyzer.Finding{{
		Category:   "forecast_query_volume",
		Severity:   severity,
		ObjectType: "database",
		Title: fmt.Sprintf("Query volume growing %.0f%%/week (week to %s)",
			w.pct, calendarDate(w.to).Format(dateLayout)),
		Detail: map[string]any{
			"forecast_type":    "query_volume",
			"wow_growth_pct":   w.pct,
			"this_week_calls":  w.thisWeek,
			"prior_week_calls": w.lastWeek,
			"window_start":     calendarDate(w.from).Format(dateLayout),
			"window_end":       calendarDate(w.to).Format(dateLayout),
		},
		Recommendation: "Query volume is rising rapidly. " +
			"Investigate new workload patterns and ensure " +
			"connection pool and CPU can handle the growth.",
	}}
}

// --- helpers ---

// sysToDataPoints converts system aggregates to regression points
// using the given metric extractor.
func sysToDataPoints(
	aggs []DaySystemAgg,
	metric func(DaySystemAgg) float64,
) []DataPoint {
	if len(aggs) == 0 {
		return nil
	}
	origin := aggs[0].Day
	points := make([]DataPoint, len(aggs))
	for i, a := range aggs {
		points[i] = DataPoint{
			// Calendar days: a 23 or 25 hour DST day is one day.
			X: float64(-dayOffset(a.Day, origin)),
			Y: metric(a),
		}
	}
	return points
}

// severityByDays returns severity based on projected days until
// threshold. Empty string means no finding needed.
func severityByDays(days float64) string {
	switch {
	case math.IsInf(days, 1):
		return ""
	case days >= 90:
		return ""
	case days < 30:
		return "critical"
	default:
		return "warning"
	}
}

// minSpanDays is the calendar days a projected series must span.
const minSpanDays = 7

// defaultMinRSquared mirrors the config default of forecaster.min_r_squared.
const defaultMinRSquared = 0.5

// minRSquared is forecaster.min_r_squared; 0, a value above 1 or NaN
// means the default.
func minRSquared(cfg ForecasterConfig) float64 {
	if v := cfg.MinRSquared; v > 0 && v <= 1 {
		return v
	}
	return defaultMinRSquared
}

// gateResult is whether a fitted series may be projected, and why.
type gateResult struct {
	ok                    bool
	spanDays, coveredDays int
}

// trendGate lets a projection through only when the fit explains at least
// forecaster.min_r_squared of the series and the series spans minSpanDays
// calendar days (forecast-08): a raw slope through noisy peaks or a
// handful of days is not a trend.
func trendGate(days []DaySystemAgg, reg RegressionResult, cfg ForecasterConfig) gateResult {
	g := gateResult{coveredDays: len(days)}
	if len(days) > 0 {
		g.spanDays = dayOffset(days[0].Day, days[len(days)-1].Day) + 1
	}
	g.ok = g.spanDays >= minSpanDays && reg.R2 >= minRSquared(cfg)-1e-12
	return g
}
