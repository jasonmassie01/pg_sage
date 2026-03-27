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

// ruleCacheHitRatio flags low buffer cache hit ratios.
func ruleCacheHitRatio(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	_ *RuleExtras,
) []Finding {
	if current.System.CacheHitRatio < 0 {
		return nil // no data
	}

	if current.System.CacheHitRatio >= cfg.Analyzer.CacheHitRatioWarning {
		return nil
	}

	severity := "warning"
	if current.System.CacheHitRatio < 0.80 {
		severity = "critical"
	}

	return []Finding{{
		Category:         "cache_hit_ratio",
		Severity:         severity,
		ObjectType:       "database",
		ObjectIdentifier: "buffer_cache",
		Title: fmt.Sprintf(
			"Cache hit ratio %.2f%% (threshold %.2f%%)",
			current.System.CacheHitRatio*100,
			cfg.Analyzer.CacheHitRatioWarning*100,
		),
		Detail: map[string]any{
			"cache_hit_ratio": current.System.CacheHitRatio,
			"threshold":       cfg.Analyzer.CacheHitRatioWarning,
		},
		Recommendation: "Increase shared_buffers or investigate heavy sequential scans.",
		ActionRisk:     "safe",
	}}
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

// ruleStatStatementsCapacity warns when tracked queries approach
// the pg_stat_statements.max limit. Warning at >80%, critical >95%.
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
	count := len(current.Queries)
	pct := float64(count) / float64(maxSt) * 100.0

	if pct < 80.0 {
		return nil
	}

	severity := "warning"
	if pct > 95.0 {
		severity = "critical"
	}

	return []Finding{{
		Category:         "stat_statements_pressure",
		Severity:         severity,
		ObjectType:       "database",
		ObjectIdentifier: "pg_stat_statements",
		Title: fmt.Sprintf(
			"pg_stat_statements at %.0f%% capacity (%d/%d)",
			pct, count, maxSt,
		),
		Detail: map[string]any{
			"tracked_queries":        count,
			"stat_statements_max":    maxSt,
			"utilization_pct":        pct,
		},
		Recommendation: "Increase pg_stat_statements.max or review tracked queries.",
		ActionRisk:     "safe",
	}}
}
