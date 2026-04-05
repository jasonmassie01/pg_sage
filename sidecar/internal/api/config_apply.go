package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// applyConfigOverrides iterates over body keys, validates, persists,
// and hot-reloads the config. Returns a list of error messages.
func applyConfigOverrides(
	ctx context.Context,
	cs *store.ConfigStore,
	cfg *config.Config,
	body map[string]any,
	databaseID int,
	userID int,
) []string {
	var errs []string
	for key, raw := range body {
		value := fmt.Sprintf("%v", raw)
		err := cs.SetOverride(
			ctx, key, value, databaseID, userID)
		if err != nil {
			errs = append(errs,
				fmt.Sprintf("%s: %s", key, err.Error()))
			continue
		}
		// Hot-reload into running config when global.
		if databaseID == 0 {
			hotReload(cfg, key, value)
		}
	}
	return errs
}

// hotReload applies a single key/value change to the in-memory
// config struct. Only hot-reloadable fields are supported.
func hotReload(cfg *config.Config, key, value string) {
	switch {
	case strings.HasPrefix(key, "collector."):
		hotReloadCollector(cfg, key, value)
	case strings.HasPrefix(key, "analyzer."):
		hotReloadAnalyzer(cfg, key, value)
	case strings.HasPrefix(key, "trust."):
		hotReloadTrust(cfg, key, value)
	case strings.HasPrefix(key, "safety."):
		hotReloadSafety(cfg, key, value)
	case strings.HasPrefix(key, "llm."):
		hotReloadLLM(cfg, key, value)
	case strings.HasPrefix(key, "advisor."):
		hotReloadAdvisor(cfg, key, value)
	case strings.HasPrefix(key, "alerting."):
		hotReloadAlerting(cfg, key, value)
	case strings.HasPrefix(key, "retention."):
		hotReloadRetention(cfg, key, value)
	}
}

func hotReloadCollector(cfg *config.Config, key, v string) {
	n, _ := strconv.Atoi(v)
	switch key {
	case "collector.interval_seconds":
		cfg.Collector.IntervalSeconds = n
	case "collector.batch_size":
		cfg.Collector.BatchSize = n
	case "collector.max_queries":
		cfg.Collector.MaxQueries = n
	}
}

func hotReloadAnalyzer(cfg *config.Config, key, v string) {
	switch key {
	case "analyzer.interval_seconds":
		cfg.Analyzer.IntervalSeconds = atoi(v)
	case "analyzer.slow_query_threshold_ms":
		cfg.Analyzer.SlowQueryThresholdMs = atoi(v)
	case "analyzer.seq_scan_min_rows":
		cfg.Analyzer.SeqScanMinRows = atoi(v)
	case "analyzer.unused_index_window_days":
		cfg.Analyzer.UnusedIndexWindowDays = atoi(v)
	case "analyzer.index_bloat_threshold_pct":
		cfg.Analyzer.IndexBloatThresholdPct = atoi(v)
	case "analyzer.table_bloat_dead_tuple_pct":
		cfg.Analyzer.TableBloatDeadTuplePct = atoi(v)
	case "analyzer.regression_threshold_pct":
		cfg.Analyzer.RegressionThresholdPct = atoi(v)
	case "analyzer.cache_hit_ratio_warning":
		cfg.Analyzer.CacheHitRatioWarning = atof(v)
	}
}

// validTrustLevels enumerates the accepted trust level strings.
var validTrustLevels = map[string]bool{
	"observation": true,
	"advisory":    true,
	"autonomous":  true,
}

func hotReloadTrust(cfg *config.Config, key, v string) {
	switch key {
	case "trust.level":
		if !validTrustLevels[v] {
			return // silently reject invalid trust levels
		}
		cfg.Trust.Level = v
	case "trust.tier3_safe":
		cfg.Trust.Tier3Safe = v == "true"
	case "trust.tier3_moderate":
		cfg.Trust.Tier3Moderate = v == "true"
	case "trust.tier3_high_risk":
		cfg.Trust.Tier3HighRisk = v == "true"
	case "trust.maintenance_window":
		cfg.Trust.MaintenanceWindow = v
	case "trust.rollback_threshold_pct":
		cfg.Trust.RollbackThresholdPct = atoi(v)
	case "trust.rollback_window_minutes":
		cfg.Trust.RollbackWindowMinutes = atoi(v)
	case "trust.rollback_cooldown_days":
		cfg.Trust.RollbackCooldownDays = atoi(v)
	case "trust.cascade_cooldown_cycles":
		cfg.Trust.CascadeCooldownCycles = atoi(v)
	}
}

func hotReloadSafety(cfg *config.Config, key, v string) {
	switch key {
	case "safety.cpu_ceiling_pct":
		cfg.Safety.CPUCeilingPct = atoi(v)
	case "safety.query_timeout_ms":
		cfg.Safety.QueryTimeoutMs = atoi(v)
	case "safety.ddl_timeout_seconds":
		cfg.Safety.DDLTimeoutSeconds = atoi(v)
	case "safety.lock_timeout_ms":
		cfg.Safety.LockTimeoutMs = atoi(v)
	}
}

func hotReloadLLM(cfg *config.Config, key, v string) {
	switch key {
	case "llm.enabled":
		cfg.LLM.Enabled = v == "true"
	case "llm.endpoint":
		cfg.LLM.Endpoint = v
	case "llm.api_key":
		cfg.LLM.APIKey = v
	case "llm.model":
		cfg.LLM.Model = v
	case "llm.timeout_seconds":
		cfg.LLM.TimeoutSeconds = atoi(v)
	case "llm.token_budget_daily":
		cfg.LLM.TokenBudgetDaily = atoi(v)
	case "llm.context_budget_tokens":
		cfg.LLM.ContextBudgetTokens = atoi(v)
	case "llm.optimizer.enabled":
		cfg.LLM.Optimizer.Enabled = v == "true"
	case "llm.optimizer.min_query_calls":
		cfg.LLM.Optimizer.MinQueryCalls = atoi(v)
	case "llm.optimizer.max_new_per_table":
		cfg.LLM.Optimizer.MaxNewPerTable = atoi(v)
	}
}

func hotReloadAdvisor(cfg *config.Config, key, v string) {
	switch key {
	case "advisor.enabled":
		cfg.Advisor.Enabled = v == "true"
	case "advisor.interval_seconds":
		cfg.Advisor.IntervalSeconds = atoi(v)
	}
}

func hotReloadAlerting(cfg *config.Config, key, v string) {
	switch key {
	case "alerting.enabled":
		cfg.Alerting.Enabled = v == "true"
	case "alerting.slack_webhook_url":
		cfg.Alerting.SlackWebhookURL = v
	case "alerting.pagerduty_routing_key":
		cfg.Alerting.PagerDutyRoutingKey = v
	case "alerting.check_interval_seconds":
		cfg.Alerting.CheckIntervalSeconds = atoi(v)
	case "alerting.cooldown_minutes":
		cfg.Alerting.CooldownMinutes = atoi(v)
	case "alerting.quiet_hours_start":
		cfg.Alerting.QuietHoursStart = v
	case "alerting.quiet_hours_end":
		cfg.Alerting.QuietHoursEnd = v
	}
}

func hotReloadRetention(cfg *config.Config, key, v string) {
	switch key {
	case "retention.snapshots_days":
		cfg.Retention.SnapshotsDays = atoi(v)
	case "retention.findings_days":
		cfg.Retention.FindingsDays = atoi(v)
	case "retention.actions_days":
		cfg.Retention.ActionsDays = atoi(v)
	case "retention.explains_days":
		cfg.Retention.ExplainsDays = atoi(v)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
