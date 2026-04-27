package api

import (
	"context"
	"fmt"
	"log/slog"
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
	type override struct {
		key   string
		value string
	}
	overrides := make([]override, 0, len(body))
	for key, raw := range body {
		value := fmt.Sprintf("%v", raw)
		if isMaskedSecretUpdate(key, value) {
			continue
		}
		if err := store.ValidateConfigOverride(key, value); err != nil {
			errs = append(errs, fmt.Sprintf(
				"%s: %s", key, configErrorMessage(err)))
			continue
		}
		overrides = append(overrides, override{key: key, value: value})
	}
	if len(errs) > 0 {
		return errs
	}

	for _, override := range overrides {
		err := cs.SetOverride(
			ctx, override.key, override.value, databaseID, userID)
		if err != nil {
			// Validation errors (invalid key, invalid value, range
			// violation) are safe to expose — the user needs them to
			// correct the request. Anything else is a DB/internal
			// failure whose text we must not leak.
			msg := configErrorMessage(err)
			if msg == internalConfigErrMsg {
				slog.Error("config override failed",
					"key", override.key, "err", err)
			}
			errs = append(errs, fmt.Sprintf("%s: %s", override.key, msg))
			continue
		}
		// Hot-reload into running config when global.
		if databaseID == 0 {
			hotReload(cfg, override.key, override.value)
		}
	}
	return errs
}

func isMaskedSecretUpdate(key, value string) bool {
	if key != "llm.api_key" || value == "" {
		return false
	}
	starCount := 0
	for starCount < len(value) && value[starCount] == '*' {
		starCount++
	}
	if starCount == 0 {
		return false
	}
	if starCount == len(value) {
		return true
	}
	return len(value)-starCount <= 4
}

// internalConfigErrMsg is the placeholder returned to clients when
// a config override fails for a reason that is not safe to expose
// (DB errors, tx failures, etc.).
const internalConfigErrMsg = "internal error"

// configErrorMessage returns a client-safe message for an error
// produced by SetOverride. Known validation error shapes are
// surfaced verbatim; anything else becomes internalConfigErrMsg.
func configErrorMessage(err error) string {
	msg := err.Error()
	// Validation error prefixes produced by store.validateConfigKey
	// and store.validateConfigValue. Keep this list in sync with
	// that package.
	safePrefixes := []string{
		"invalid config key",
		"invalid value",
		"value below minimum",
		"value above maximum",
		"must be one of",
		"unknown config key",
	}
	for _, p := range safePrefixes {
		if strings.Contains(msg, p) {
			return msg
		}
	}
	return internalConfigErrMsg
}

// hotReload applies a single key/value change to the in-memory
// config struct. Only hot-reloadable fields are supported.
//
// Takes the config write lock so concurrent mutations are serialized
// and readers that opt into cfg.Mu.RLock() observe a consistent state.
func hotReload(cfg *config.Config, key, value string) {
	config.LockForHotReload()
	defer config.UnlockForHotReload()
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
	case strings.HasPrefix(key, "briefing."):
		hotReloadBriefing(cfg, key, value)
	case strings.HasPrefix(key, "forecaster."):
		hotReloadForecaster(cfg, key, value)
	case strings.HasPrefix(key, "auto_explain."):
		hotReloadAutoExplain(cfg, key, value)
	case strings.HasPrefix(key, "tuner."):
		hotReloadTuner(cfg, key, value)
	case strings.HasPrefix(key, "rca."):
		hotReloadRCA(cfg, key, value)
	case strings.HasPrefix(key, "runaway."):
		hotReloadRunaway(cfg, key, value)
	case strings.HasPrefix(key, "explain."):
		hotReloadExplain(cfg, key, value)
	case strings.HasPrefix(key, "logwatch."):
		hotReloadLogWatch(cfg, key, value)
	case strings.HasPrefix(key, "schema_lint."):
		hotReloadSchemaLint(cfg, key, value)
	case strings.HasPrefix(key, "migration."):
		hotReloadMigration(cfg, key, value)
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
	case "analyzer.slow_slot_retained_bytes":
		cfg.Analyzer.SlowSlotRetainedBytes = atoi64(v)
	case "analyzer.lock_chain.enabled":
		cfg.Analyzer.LockChain.Enabled = v == "true"
	case "analyzer.lock_chain.min_blocked_threshold":
		cfg.Analyzer.LockChain.MinBlockedThreshold = atoi(v)
	case "analyzer.lock_chain.critical_blocked_threshold":
		cfg.Analyzer.LockChain.CriticalBlockedThreshold = atoi(v)
	case "analyzer.lock_chain.idle_in_tx_terminate_minutes":
		cfg.Analyzer.LockChain.IdleInTxTerminateMinutes = atoi(v)
	case "analyzer.lock_chain.active_query_cancel_minutes":
		cfg.Analyzer.LockChain.ActiveQueryCancelMinutes = atoi(v)
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
	case "llm.json_mode":
		cfg.LLM.JSONMode = v == "true"
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
	case "llm.optimizer_llm.enabled":
		cfg.LLM.OptimizerLLM.Enabled = v == "true"
	case "llm.optimizer_llm.endpoint":
		cfg.LLM.OptimizerLLM.Endpoint = v
	case "llm.optimizer_llm.model":
		cfg.LLM.OptimizerLLM.Model = v
	case "llm.optimizer_llm.token_budget_daily":
		cfg.LLM.OptimizerLLM.TokenBudgetDaily = atoi(v)
	case "llm.optimizer_llm.max_output_tokens":
		cfg.LLM.OptimizerLLM.MaxOutputTokens = atoi(v)
	case "llm.optimizer_llm.fallback_to_general":
		cfg.LLM.OptimizerLLM.FallbackToGeneral = v == "true"
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
	case "alerting.timezone":
		cfg.Alerting.Timezone = v
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

func hotReloadBriefing(cfg *config.Config, key, v string) {
	switch key {
	case "briefing.schedule":
		cfg.Briefing.Schedule = v
	case "briefing.slack_webhook_url":
		cfg.Briefing.SlackWebhookURL = v
	}
}

func hotReloadForecaster(cfg *config.Config, key, v string) {
	switch key {
	case "forecaster.enabled":
		cfg.Forecaster.Enabled = v == "true"
	case "forecaster.lookback_days":
		cfg.Forecaster.LookbackDays = atoi(v)
	case "forecaster.disk_warn_growth_gb_day":
		cfg.Forecaster.DiskWarnGrowthGBDay = atof(v)
	}
}

func hotReloadAutoExplain(cfg *config.Config, key, v string) {
	switch key {
	case "auto_explain.enabled":
		cfg.AutoExplain.Enabled = v == "true"
	case "auto_explain.log_min_duration_ms":
		cfg.AutoExplain.LogMinDurationMs = atoi(v)
	case "auto_explain.collect_interval_seconds":
		cfg.AutoExplain.CollectIntervalSeconds = atoi(v)
	case "auto_explain.max_plans_per_cycle":
		cfg.AutoExplain.MaxPlansPerCycle = atoi(v)
	}
}

func hotReloadTuner(cfg *config.Config, key, v string) {
	switch key {
	case "tuner.enabled":
		cfg.Tuner.Enabled = v == "true"
	case "tuner.llm_enabled":
		cfg.Tuner.LLMEnabled = v == "true"
	case "tuner.work_mem_max_mb":
		cfg.Tuner.WorkMemMaxMB = atoi(v)
	case "tuner.plan_time_ratio":
		cfg.Tuner.PlanTimeRatio = atof(v)
	case "tuner.nested_loop_row_threshold":
		n, _ := strconv.ParseInt(v, 10, 64)
		cfg.Tuner.NestedLoopRowThreshold = n
	case "tuner.parallel_min_table_rows":
		n, _ := strconv.ParseInt(v, 10, 64)
		cfg.Tuner.ParallelMinTableRows = n
	case "tuner.min_query_calls":
		cfg.Tuner.MinQueryCalls = atoi(v)
	case "tuner.verify_after_apply":
		cfg.Tuner.VerifyAfterApply = v == "true"
	}
}

func hotReloadRCA(cfg *config.Config, key, v string) {
	switch key {
	case "rca.enabled":
		cfg.RCA.Enabled = v == "true"
	case "rca.llm_correlation_threshold":
		cfg.RCA.LLMCorrelationThreshold = atoi(v)
	case "rca.dedup_window_minutes":
		cfg.RCA.DedupWindowMinutes = atoi(v)
	case "rca.escalation_cycles":
		cfg.RCA.EscalationCycles = atoi(v)
	case "rca.resolution_cycles":
		cfg.RCA.ResolutionCycles = atoi(v)
	case "rca.connection_saturation_pct":
		cfg.RCA.ConnectionSaturationPct = atoi(v)
	case "rca.replication_lag_threshold_seconds":
		cfg.RCA.ReplicationLagThresholdS = atoi(v)
	case "rca.wal_spike_multiplier":
		cfg.RCA.WALSpikeMultiplier = atof(v)
	}
}

func hotReloadRunaway(cfg *config.Config, key, v string) {
	switch key {
	case "runaway.enabled":
		cfg.Runaway.Enabled = v == "true"
	}
}

func hotReloadExplain(cfg *config.Config, key, v string) {
	switch key {
	case "explain.enabled":
		cfg.Explain.Enabled = v == "true"
	case "explain.timeout_ms":
		cfg.Explain.TimeoutMs = atoi(v)
	case "explain.cache_ttl_minutes":
		cfg.Explain.CacheTTLMinutes = atoi(v)
	case "explain.max_tokens":
		cfg.Explain.MaxTokens = atoi(v)
	}
}

func hotReloadLogWatch(cfg *config.Config, key, v string) {
	switch key {
	case "logwatch.enabled":
		cfg.LogWatch.Enabled = v == "true"
	case "logwatch.log_directory":
		cfg.LogWatch.LogDirectory = v
	case "logwatch.format":
		cfg.LogWatch.Format = v
	case "logwatch.poll_interval_ms":
		cfg.LogWatch.PollIntervalMs = atoi(v)
	case "logwatch.dedup_window_seconds":
		cfg.LogWatch.DedupWindowS = atoi(v)
	case "logwatch.max_line_len_bytes":
		cfg.LogWatch.MaxLineLenBytes = atoi(v)
	case "logwatch.temp_file_min_bytes":
		cfg.LogWatch.TempFileMinBytes = atoi(v)
	case "logwatch.max_lines_per_cycle":
		cfg.LogWatch.MaxLinesPerCycle = atoi(v)
	case "logwatch.exclude_applications":
		cfg.LogWatch.ExcludeApplications = strings.Split(v, ",")
	case "logwatch.slow_query_enabled":
		cfg.LogWatch.SlowQueryEnabled = v == "true"
	}
}

func hotReloadSchemaLint(cfg *config.Config, key, v string) {
	switch key {
	case "schema_lint.enabled":
		cfg.SchemaLint.Enabled = v == "true"
	case "schema_lint.scan_interval_minutes":
		cfg.SchemaLint.ScanIntervalMinutes = atoi(v)
	case "schema_lint.min_table_rows":
		cfg.SchemaLint.MinTableRows = atoi(v)
	}
}

func hotReloadMigration(cfg *config.Config, key, v string) {
	switch key {
	case "migration.enabled":
		cfg.Migration.Enabled = v == "true"
	case "migration.mode":
		cfg.Migration.Mode = v
	case "migration.managed_service":
		cfg.Migration.ManagedService = v
	case "migration.log_detection":
		cfg.Migration.LogDetection = v == "true"
	case "migration.activity_polling":
		cfg.Migration.ActivityPolling = v == "true"
	case "migration.poll_interval_seconds":
		cfg.Migration.PollIntervalSeconds = atoi(v)
	case "migration.ddl_row_threshold":
		cfg.Migration.DDLRowThreshold = atoi(v)
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		slog.Warn("hotReload: invalid int",
			"value", s, "error", err)
		return 0
	}
	return n
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		slog.Warn("hotReload: invalid int64",
			"value", s, "error", err)
		return 0
	}
	return n
}

func atof(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("hotReload: invalid float",
			"value", s, "error", err)
		return 0
	}
	return f
}
