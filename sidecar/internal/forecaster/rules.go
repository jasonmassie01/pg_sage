package forecaster

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

const minDataPoints = 7

// forecastDiskGrowth warns when database size is growing faster
// than the configured threshold in GB/day.
func forecastDiskGrowth(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	if len(aggs) < minDataPoints {
		return nil
	}

	points := sysToDataPoints(aggs, func(a DaySystemAgg) float64 {
		return a.MaxDBSizeBytes
	})
	reg := LinearRegression(points)

	gbPerDay := reg.Slope / 1e9
	if gbPerDay <= cfg.DiskWarnGrowthGBDay {
		return nil
	}

	currentGB := aggs[len(aggs)-1].MaxDBSizeBytes / 1e9
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

// forecastConnectionSaturation projects when active connections
// will reach a dangerous fraction of max_connections.
func forecastConnectionSaturation(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	if len(aggs) < minDataPoints {
		return nil
	}

	points := sysToDataPoints(aggs, func(a DaySystemAgg) float64 {
		return a.MaxActiveBackends
	})
	reg := LinearRegression(points)

	latest := aggs[len(aggs)-1]
	maxConns := latest.MaxConnections
	if maxConns == 0 {
		return nil
	}

	threshold := maxConns * cfg.ConnectionWarnPct / 100
	current := latest.MaxActiveBackends
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
			"current_active":   current,
			"max_connections":  maxConns,
			"slope_per_day":    reg.Slope,
			"days_to_warn_pct": days,
			"r_squared":        reg.R2,
		},
		Recommendation: fmt.Sprintf(
			"Active connections are trending toward %.0f%% "+
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

// forecastSequenceExhaustion projects when sequences will reach
// 100% usage based on linear trend.
func forecastSequenceExhaustion(
	seqAggs []DaySeqAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	grouped := groupSeqAggs(seqAggs)

	var findings []analyzer.Finding
	for name, aggs := range grouped {
		if len(aggs) < minDataPoints {
			continue
		}

		points := seqToDataPoints(aggs)
		reg := LinearRegression(points)
		current := aggs[len(aggs)-1].PctUsed
		days := DaysUntilThreshold(current, reg.Slope, 100.0)

		severity := seqSeverity(days, cfg)
		if severity == "" {
			continue
		}

		findings = append(findings, analyzer.Finding{
			Category:         "forecast_sequence_exhaustion",
			Severity:         severity,
			ObjectType:       "sequence",
			ObjectIdentifier: name,
			Title: fmt.Sprintf(
				"Sequence %s will exhaust in %.0f days",
				name, days,
			),
			Detail: map[string]any{
				"forecast_type": "sequence_exhaustion",
				"current_pct":   current,
				"slope_per_day": reg.Slope,
				"days_until":    days,
				"r_squared":     reg.R2,
			},
			Recommendation: fmt.Sprintf(
				"Sequence %s is projected to exhaust. "+
					"Consider ALTER SEQUENCE ... MAXVALUE "+
					"or switching to bigint.",
				name,
			),
		})
	}
	return findings
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
// trending upward.
func forecastCheckpointPressure(
	aggs []DaySystemAgg, cfg ForecasterConfig,
) []analyzer.Finding {
	if len(aggs) < minDataPoints+1 {
		return nil
	}

	rates := make([]float64, 0, len(aggs)-1)
	for i := 1; i < len(aggs); i++ {
		delta := aggs[i].TotalCheckpoints -
			aggs[i-1].TotalCheckpoints
		if delta < 0 {
			delta = 0 // stats reset
		}
		rates = append(rates, delta/24) // per hour
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

// seqToDataPoints converts sequence aggregates to regression points.
func seqToDataPoints(aggs []DaySeqAgg) []DataPoint {
	if len(aggs) == 0 {
		return nil
	}
	origin := aggs[0].Day
	points := make([]DataPoint, len(aggs))
	for i, a := range aggs {
		points[i] = DataPoint{
			X: a.Day.Sub(origin).Hours() / 24,
			Y: a.PctUsed,
		}
	}
	return points
}

// groupSeqAggs groups sequence aggregates by sequence name.
func groupSeqAggs(
	aggs []DaySeqAgg,
) map[string][]DaySeqAgg {
	m := make(map[string][]DaySeqAgg)
	for _, a := range aggs {
		m[a.SeqName] = append(m[a.SeqName], a)
	}
	return m
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

// seqSeverity determines severity for sequence exhaustion.
func seqSeverity(days float64, cfg ForecasterConfig) string {
	switch {
	case math.IsInf(days, 1):
		return ""
	case days <= float64(cfg.SequenceCriticalDays):
		return "critical"
	case days <= float64(cfg.SequenceWarnDays):
		return "warning"
	default:
		return ""
	}
}
