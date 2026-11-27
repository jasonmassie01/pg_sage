package forecaster

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// The cache forecast (forecast-04) judges each closed day's interval hit
// ratio: the blks_hit share of the blocks accessed between snapshots that
// day. The lifetime ratio of pg_stat_database hid a working set that
// outgrew shared_buffers for weeks, and its monotone drift read as a
// decline.
const (
	// defaultCacheMinBlocksPerDay mirrors the config default of
	// forecaster.cache_min_blocks_per_day for a caller that leaves it unset.
	defaultCacheMinBlocksPerDay = 100_000
	// cacheEWMAAlpha weighs the recent days: the EWMA shown beside the
	// newest day must not be the oldest day's value.
	cacheEWMAAlpha = 0.3
	// cacheDeclineMinDrop is the fitted drop across the window, a fraction
	// (1 percentage point), at which "declining" warns.
	cacheDeclineMinDrop = 0.01
	// cacheDropTolerance absorbs float error at exactly 1 pp.
	cacheDropTolerance = 1e-9
	// cacheDeclineMinR2 is the fit a decline needs to be a trend.
	cacheDeclineMinR2 = 0.5
)

// cacheMinBlocks is forecaster.cache_min_blocks_per_day, defaulted.
func cacheMinBlocks(cfg ForecasterConfig) int64 {
	if cfg.CacheMinBlocksPerDay > 0 {
		return cfg.CacheMinBlocksPerDay
	}
	return defaultCacheMinBlocksPerDay
}

// gateCacheDays marks unknown the days with fewer than minBlocks block
// accesses: a ratio over a handful of blocks is noise, not pressure.
func gateCacheDays(aggs []DaySystemAgg, minBlocks int64) {
	for i := range aggs {
		if aggs[i].CacheBlocksAccessed < float64(minBlocks) {
			aggs[i].CacheUnknown = true
		}
	}
}

// knownCacheDays returns the days with a usable ratio, normalised to a
// fraction (a legacy percent is divided by 100). Unknown days and values
// outside [0, 1] after normalising (negative, NaN, infinite) are left
// out, so they never reach the EWMA or the regression.
func knownCacheDays(aggs []DaySystemAgg) []DaySystemAgg {
	known := make([]DaySystemAgg, 0, len(aggs))
	for _, a := range aggs {
		if a.CacheUnknown {
			continue
		}
		r := analyzer.NormalizeCacheHitRatio(a.CacheHitRatio)
		if !(r >= 0 && r <= 1) {
			continue
		}
		a.CacheHitRatio = r
		known = append(known, a)
	}
	return known
}

// cacheEvaluable reports whether minDataPoints days have a usable ratio.
// Without them (legacy rows lack the interval counters, a fresh install,
// quiet days) the category is not evaluated, so an open finding is
// neither refreshed nor resolved.
func cacheEvaluable(aggs []DaySystemAgg) bool {
	return len(knownCacheDays(aggs)) >= minDataPoints
}

// forecastCachePressure warns "low" when the newest usable day's hit
// ratio is below forecaster.cache_warn_threshold (the EWMA shown beside
// it), and "declining" when the fitted drop across the usable days is at
// least 1 percentage point with R² above 0.5.
func forecastCachePressure(aggs []DaySystemAgg, cfg ForecasterConfig) []analyzer.Finding {
	known := knownCacheDays(aggs)
	if len(known) < minDataPoints {
		return nil
	}
	ratio := func(a DaySystemAgg) float64 { return a.CacheHitRatio }
	ratios := make([]float64, len(known))
	for i, a := range known {
		ratios[i] = ratio(a)
	}
	c := cacheCase{newest: known[len(known)-1], days: len(known),
		ewma: EWMA(ratios, cacheEWMAAlpha)}
	var findings []analyzer.Finding
	if c.newest.CacheHitRatio < cfg.CacheWarnThreshold {
		findings = append(findings, cacheLowFinding(c, cfg.CacheWarnThreshold))
	}
	reg := LinearRegression(sysToDataPoints(known, ratio))
	span := c.newest.Day.Sub(known[0].Day).Hours() / 24
	if drop := -reg.Slope * span; drop >= cacheDeclineMinDrop-cacheDropTolerance &&
		reg.R2 > cacheDeclineMinR2 {
		findings = append(findings, cacheDecliningFinding(c, reg, drop))
	}
	return findings
}

// cacheCase is what both cache findings report.
type cacheCase struct {
	newest DaySystemAgg
	days   int
	ewma   float64
}

// detail is the evidence common to both findings.
func (c cacheCase) detail(kind string) map[string]any {
	return map[string]any{
		"forecast_type":   kind,
		"day":             c.newest.Day,
		"day_hit_ratio":   c.newest.CacheHitRatio,
		"blocks_accessed": c.newest.CacheBlocksAccessed,
		"ewma_value":      c.ewma,
		"days_evaluated":  c.days,
	}
}

func cacheLowFinding(c cacheCase, threshold float64) analyzer.Finding {
	d := c.detail("cache_pressure")
	d["threshold"] = threshold
	return analyzer.Finding{
		Category:   "forecast_cache_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf("Cache hit ratio low: %.1f%% on the newest measured day "+
			"(EWMA %.1f%%)", c.newest.CacheHitRatio*100, c.ewma*100),
		Detail: d,
		Recommendation: "The newest measured day read more blocks from outside " +
			"shared_buffers than the threshold allows. Compare the listed tables' " +
			"working set with shared_buffers: add indexes or change the queries " +
			"that scan them, or raise shared_buffers (a restart).",
	}
}

func cacheDecliningFinding(c cacheCase, reg RegressionResult, drop float64) analyzer.Finding {
	d := c.detail("cache_decline")
	d["slope_per_day"] = reg.Slope
	d["r_squared"] = reg.R2
	d["drop_pp"] = drop * 100
	return analyzer.Finding{
		Category:   "forecast_cache_pressure",
		Severity:   "warning",
		ObjectType: "database",
		Title: fmt.Sprintf("Cache hit ratio declined %.1f points over %d days, now %.1f%%",
			drop*100, c.days, c.newest.CacheHitRatio*100),
		Detail: d,
		Recommendation: "The daily cache hit ratio is falling: the working set is " +
			"growing past shared_buffers. Review the listed tables' growth and " +
			"access patterns before the ratio crosses the threshold.",
	}
}
