package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// LeakedConn represents a connection stuck in "idle in transaction" state.
type LeakedConn struct {
	PID          int
	UserName     string
	AppName      string
	IdleDuration string
}

// ruleConnectionLeaks flags connections that have been idle in transaction
// longer than the configured timeout.
func ruleConnectionLeaks(leakedConns []LeakedConn) []Finding {
	var findings []Finding
	for _, c := range leakedConns {
		ident := fmt.Sprintf("pid:%d", c.PID)
		findings = append(findings, Finding{
			Category:         "connection_leak",
			Severity:         "warning",
			ObjectType:       "connection",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Idle-in-transaction connection PID %d (%s/%s) for %s",
				c.PID, c.UserName, c.AppName, c.IdleDuration,
			),
			Detail: map[string]any{
				"pid":           c.PID,
				"usename":       c.UserName,
				"application":   c.AppName,
				"idle_duration": c.IdleDuration,
			},
			Recommendation: "Terminate the leaked connection.",
			RecommendedSQL: fmt.Sprintf(
				"SELECT pg_terminate_backend(%d);", c.PID,
			),
			ActionRisk: "safe",
		})
	}
	return findings
}

// cacheRatioMinBlocks is the fewest block accesses an interval needs
// before its hit ratio is evidence (collection-06): a quiet minute of a
// few hundred reads says nothing about the cache.
const cacheRatioMinBlocks = 10_000

// IntervalCacheRatio is the interval's cache hit ratio and block count
// when the interval is evidence: measured, with at least
// cacheRatioMinBlocks accessed.
func IntervalCacheRatio(s *collector.Snapshot) (float64, int64, bool) {
	iv := s.System.Interval
	if iv == nil || iv.CacheHitRatio == nil || iv.BlksAccessed == nil ||
		*iv.BlksAccessed < cacheRatioMinBlocks {
		return 0, 0, false
	}
	return *iv.CacheHitRatio, *iv.BlksAccessed, true
}

// ruleCacheHitRatio flags a low buffer cache hit ratio over the last
// collector interval. The lifetime ratio (since the last
// pg_stat_database reset, often months) barely moves when a working set
// outgrows shared_buffers, so it is reported, never judged.
func ruleCacheHitRatio(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	ratio, blocks, ok := IntervalCacheRatio(current)
	if !ok || ratio >= cfg.Analyzer.CacheHitRatioWarning {
		return nil
	}
	severity := "warning"
	if ratio < 0.80 {
		severity = "critical"
	}
	iv := current.System.Interval
	var lifetime any
	if l := NormalizeCacheHitRatio(current.System.CacheHitRatio); l >= 0 {
		lifetime = l
	}
	return []Finding{{
		Category:         "cache_hit_ratio",
		Severity:         severity,
		ObjectType:       "database",
		ObjectIdentifier: "buffer_cache",
		Title: fmt.Sprintf(
			"Cache hit ratio %.2f%% over the last %s (threshold %.2f%%)",
			ratio*100, intervalLabel(iv.Seconds),
			cfg.Analyzer.CacheHitRatioWarning*100,
		),
		Detail: map[string]any{
			"cache_hit_ratio":          ratio,
			"lifetime_cache_hit_ratio": lifetime,
			"blocks_accessed":          blocks,
			"interval_seconds":         iv.Seconds,
			"threshold":                cfg.Analyzer.CacheHitRatioWarning,
		},
		Recommendation: "Increase shared_buffers or investigate heavy sequential scans.",
		ActionRisk:     "safe",
	}}
}

// intervalLabel renders an interval length in whole seconds ("60s").
func intervalLabel(seconds float64) string {
	return fmt.Sprintf("%.0fs", seconds)
}

// ruleCheckpointPressure detects excessive checkpoint frequency.
// It compares current and previous system snapshots to calculate
// checkpoint rate. Skips on first cycle when previous is nil.
func ruleCheckpointPressure(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if previous == nil {
		return nil
	}
	if current.CollectedAt.IsZero() || previous.CollectedAt.IsZero() {
		return nil
	}

	elapsed := current.CollectedAt.Sub(previous.CollectedAt)
	if elapsed.Seconds() < 60 {
		return nil
	}

	delta := current.System.TotalCheckpoints - previous.System.TotalCheckpoints
	if delta <= 0 {
		return nil
	}

	perHour := float64(delta) / elapsed.Hours()
	threshold := float64(cfg.Analyzer.CheckpointFreqWarningPerHour)
	if perHour <= threshold {
		return nil
	}

	return []Finding{{
		Category:         "checkpoint_pressure",
		Severity:         "warning",
		ObjectType:       "database",
		ObjectIdentifier: "checkpoints",
		Title: fmt.Sprintf(
			"High checkpoint frequency: %.1f/hr (threshold %d/hr)",
			perHour, cfg.Analyzer.CheckpointFreqWarningPerHour,
		),
		Detail: map[string]any{
			"checkpoints_delta": delta,
			"elapsed_seconds":   elapsed.Seconds(),
			"per_hour":          perHour,
			"threshold":         threshold,
		},
		Recommendation: "Increase checkpoint_completion_target or max_wal_size.",
		ActionRisk:     "safe",
	}}
}

// ruleStatStatementsCapacity warns when pg_stat_statements approaches
// pg_stat_statements.max (warning at 80%, critical above 95%), counting
// its true entries when the collector read them (the collected queries
// are only the top N). Near capacity and mostly utility statements
// (pg_dump's COPY ... TO stdout), it says so and what to change.
func ruleStatStatementsCapacity(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	maxSt := current.System.StatStatementsMax
	if maxSt <= 0 {
		return nil
	}
	usage := current.System.StatStatements
	count := len(current.Queries)
	if usage != nil && usage.Entries > count {
		count = usage.Entries
	}
	pct := float64(count) / float64(maxSt) * 100.0
	if pct < 80.0 {
		return nil
	}
	severity := "warning"
	if pct > 95.0 {
		severity = "critical"
	}
	detail := map[string]any{
		"tracked_queries":     count,
		"stat_statements_max": maxSt,
		"utilization_pct":     pct,
	}
	title := fmt.Sprintf("pg_stat_statements at %.0f%% capacity (%d/%d)", pct, count, maxSt)
	if usage != nil {
		title += usageDetail(usage, detail)
	}
	return []Finding{{
		Category:         "stat_statements_pressure",
		Severity:         severity,
		ObjectType:       "database",
		ObjectIdentifier: "pg_stat_statements",
		Title:            title,
		Detail:           detail,
		Recommendation:   capacityAdvice(usage),
		ActionRisk:       "safe",
	}}
}
