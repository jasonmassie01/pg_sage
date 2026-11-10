package forecaster

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

const minDataPoints = 7

// forecastDiskGrowth warns when database size is growing faster
// than the configured threshold in GB/day. Only days with a measured
// size count, and minDataPoints of them are needed (forecast-07).
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

	gbPerDay := reg.Slope / 1e9
	if gbPerDay <= cfg.DiskWarnGrowthGBDay {
		return nil
	}

	currentGB := known[len(known)-1].MaxDBSizeBytes / 1e9
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
		},
		Recommendation: fmt.Sprintf(
			"Disk is growing at %.1f GB/day. "+
				"Review table bloat, TOAST usage, and "+
				"retention policies.", gbPerDay,
		),
	}}
}

// connectionsInUse counts every backend holding a connection slot. Idle
// and idle-in-transaction sessions consume slots too, so saturation is
// measured on total backends, not active ones (G2-B18).
func connectionsInUse(a DaySystemAgg) float64 {
	return math.Max(a.MaxTotalBackends, a.MaxActiveBackends)
}

// forecastConnectionSaturation projects when connections in use
// will reach a dangerous fraction of max_connections. Days without
// backend counts or max_connections are skipped (forecast-07).
func forecastConnectionSaturation(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	known := knownConnectionDays(aggs)
	if len(known) < minDataPoints {
		return nil
	}

	reg := LinearRegression(sysToDataPoints(known, connectionsInUse))

	latest := known[len(known)-1]
	maxConns := latest.MaxConnections
	if maxConns == 0 {
		return nil
	}

	threshold := maxConns * cfg.ConnectionWarnPct / 100
	current := connectionsInUse(latest)
	days := DaysUntilThreshold(current, reg.Slope, threshold)

	severity := severityByDays(days)
	if severity == "" {
		return nil
	}

	return []analyzer.Finding{{
		Category:   "forecast_connection_saturation",
		Severity:   severity,
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Peak connections projected to reach %.0f%% "+
				"of max_connections in %.0f days",
			cfg.ConnectionWarnPct, days,
		),
		Detail: map[string]any{
			"forecast_type":    "connection_saturation",
			"current_in_use":   current,
			"max_connections":  maxConns,
			"slope_per_day":    reg.Slope,
			"days_to_warn_pct": days,
			"r_squared":        reg.R2,
		},
		Recommendation: fmt.Sprintf(
			"Connections in use (active + idle) are trending toward %.0f%% "+
				"of max_connections. Consider connection "+
				"pooling or increasing max_connections.",
			cfg.ConnectionWarnPct,
		),
	}}
}

// forecastQueryVolume flags rapid week-over-week query growth.
func forecastQueryVolume(
	qAggs []DayQueryAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	if len(qAggs) < 14 {
		return nil
	}

	values := make([]float64, len(qAggs))
	for i, a := range qAggs {
		values[i] = a.TotalCalls
	}

	growth := WeekOverWeekGrowthPct(values)

	var severity string
	switch {
	case growth > 100:
		severity = "critical"
	case growth > 50:
		severity = "warning"
	default:
		return nil
	}

	return []analyzer.Finding{{
		Category:   "forecast_query_volume",
		Severity:   severity,
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Query volume growing %.0f%%/week", growth,
		),
		Detail: map[string]any{
			"forecast_type":  "query_volume",
			"wow_growth_pct": growth,
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
			X: a.Day.Sub(origin).Hours() / 24,
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
