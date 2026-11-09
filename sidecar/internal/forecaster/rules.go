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

// forecastCachePressure detects low or declining buffer cache hit ratio.
// Ratios are fractions (0-1); legacy percent rows are normalised with
// analyzer.NormalizeCacheHitRatio (C01).
func forecastCachePressure(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	if len(aggs) < minDataPoints {
		return nil
	}
	values := make([]float64, len(aggs))
	for i, a := range aggs {
		values[i] = analyzer.NormalizeCacheHitRatio(a.AvgCacheHitRatio)
	}
	smoothed := EWMA(values, 0.1)

	var findings []analyzer.Finding
	if smoothed < cfg.CacheWarnThreshold {
		findings = append(findings,
			cacheLowFinding(smoothed, cfg.CacheWarnThreshold))
	}
	points := sysToDataPoints(aggs, func(a DaySystemAgg) float64 {
		return analyzer.NormalizeCacheHitRatio(a.AvgCacheHitRatio)
	})
	reg := LinearRegression(points)
	if reg.Slope < 0 && reg.R2 > 0.5 {
		findings = append(findings, cacheDecliningFinding(smoothed, reg))
	}
	return findings
}

func cacheLowFinding(smoothed, threshold float64) analyzer.Finding {
	return analyzer.Finding{
		Category:   "forecast_cache_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Cache hit ratio low, EWMA at %.1f%%", smoothed*100,
		),
		Detail: map[string]any{
			"forecast_type": "cache_pressure",
			"ewma_value":    smoothed,
			"threshold":     threshold,
		},
		Recommendation: "Cache hit ratio is below threshold. " +
			"Consider increasing shared_buffers or " +
			"reviewing query patterns.",
	}
}

func cacheDecliningFinding(smoothed float64, reg RegressionResult) analyzer.Finding {
	return analyzer.Finding{
		Category:   "forecast_cache_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Cache hit ratio declining, trending toward %.1f%%",
			smoothed*100,
		),
		Detail: map[string]any{
			"forecast_type": "cache_decline",
			"slope_per_day": reg.Slope,
			"r_squared":     reg.R2,
			"ewma_value":    smoothed,
		},
		Recommendation: "Cache hit ratio shows a declining " +
			"trend. Monitor shared_buffers usage and " +
			"working set size.",
	}
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

// forecastCheckpointPressure warns when the checkpoint rate is
// trending upward. Days without a checkpoint count are skipped, and a
// rate spans the days elapsed since the previous counted day (forecast-07).
func forecastCheckpointPressure(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	known := knownDays(aggs, func(a DaySystemAgg) bool {
		return a.CheckpointsUnknown
	})
	if len(known) < minDataPoints+1 {
		return nil
	}

	rates := make([]float64, 0, len(known)-1)
	for i := 1; i < len(known); i++ {
		delta := known[i].TotalCheckpoints -
			known[i-1].TotalCheckpoints
		if delta < 0 {
			delta = 0 // stats reset
		}
		hours := 24 * elapsedDays(known[i-1].Day, known[i].Day)
		rates = append(rates, delta/hours) // per hour
	}

	smoothed := EWMA(rates, 0.1)
	if smoothed <= 12 {
		return nil
	}

	return []analyzer.Finding{{
		Category:   "forecast_checkpoint_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf(
			"Checkpoint rate trending to %.1f/hr", smoothed,
		),
		Detail: map[string]any{
			"forecast_type": "checkpoint_pressure",
			"ewma_rate_hr":  smoothed,
			"daily_rates":   rates,
		},
		Recommendation: "Checkpoint rate is high. Consider " +
			"increasing checkpoint_completion_target or " +
			"max_wal_size to reduce checkpoint frequency.",
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
