package api

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// validatedConfigWrites checks each key and value, including the target
// policy for URLs alerting connects to (alerting-11).
func validatedConfigWrites(
	body map[string]any, policy notify.TargetPolicy,
) ([]store.ConfigOverrideWrite, []string) {
	writes := make([]store.ConfigOverrideWrite, 0, len(body))
	var errs []string
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
		if err := checkConfigTarget(key, value, policy); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		writes = append(writes, store.ConfigOverrideWrite{
			Key: key, Value: value,
		})
	}
	sort.Slice(writes, func(i, j int) bool {
		return writes[i].Key < writes[j].Key
	})
	return writes, errs
}

func isMaskedSecretUpdate(key, value string) bool {
	if (key != "llm.api_key" && key != "clone.dle_token") || value == "" {
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
	case strings.HasPrefix(key, "policy."),
		strings.HasPrefix(key, "value."),
		strings.HasPrefix(key, "verify."),
		strings.HasPrefix(key, "clone."),
		strings.HasPrefix(key, "custodian."),
		strings.HasPrefix(key, "mcp."):
		hotReloadAgentNative(cfg, key, value)
	}
}

// ApplyConfigOverrideSnapshot applies one persisted override that
// store.ReplayOverride accepted while assembling a snapshot. It must be
// called before workers start; ReplayStoredOverrides is the validating loop.
func ApplyConfigOverrideSnapshot(
	candidate *config.Config, key, value string,
) {
	hotReload(candidate, key, value)
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
	case "analyzer.self_cost_budget_ms":
		cfg.Analyzer.SelfCostBudgetMs = atoi(v)
	case "analyzer.inactive_slot_min_minutes":
		cfg.Analyzer.InactiveSlotMinMinutes = atoi(v)
	case "analyzer.seq_scan_min_rows":
		cfg.Analyzer.SeqScanMinRows = atoi(v)
	case "analyzer.unused_index_window_days":
		cfg.Analyzer.UnusedIndexWindowDays = atoi(v)
	case "analyzer.index_bloat_threshold_pct":
		cfg.Analyzer.IndexBloatThresholdPct = atoi(v)
	case "analyzer.table_bloat_dead_tuple_pct":
		cfg.Analyzer.TableBloatDeadTuplePct = atoi(v)
	case "analyzer.autovacuum_tune_min_rows":
		cfg.Analyzer.AutovacuumTuneMinRows = atoi(v)
	case "analyzer.analyze_stale_min_rows":
		cfg.Analyzer.AnalyzeStaleMinRows = atoi(v)
	case "analyzer.analyze_stale_days":
		cfg.Analyzer.AnalyzeStaleDays = atoi(v)
	case "analyzer.wraparound_freeze_share":
		cfg.Analyzer.WraparoundFreezeShare = atof(v)
	case "analyzer.regression_threshold_pct":
		cfg.Analyzer.RegressionThresholdPct = atoi(v)
	case "analyzer.cache_hit_ratio_warning":
		cfg.Analyzer.CacheHitRatioWarning = atof(v)
	case "analyzer.disabled_rules":
		cfg.Analyzer.DisabledRules = config.ParseRuleList(v)
	case "analyzer.slow_slot_retained_bytes":
		cfg.Analyzer.SlowSlotRetainedBytes = atoi64(v)
	case "analyzer.lock_chain.enabled":
		cfg.Analyzer.LockChain.Enabled = atob(v)
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

func hotReloadTrust(cfg *config.Config, key, v string) {
	switch key {
	case "trust.level":
		if !config.IsTrustLevel(v) {
			return // replay validated it; never apply an unknown level
		}
		cfg.Trust.Level = v
	case "trust.tier3_safe":
		cfg.Trust.Tier3Safe = atob(v)
	case "trust.tier3_moderate":
		cfg.Trust.Tier3Moderate = atob(v)
	case "trust.tier3_high_risk":
		cfg.Trust.Tier3HighRisk = atob(v)
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
	case "safety.host_cpu_ceiling_pct":
		cfg.Safety.HostCPUCeilingPct = atoi(v)
	case "safety.query_timeout_ms":
		cfg.Safety.QueryTimeoutMs = atoi(v)
	case "safety.ddl_timeout_seconds":
		cfg.Safety.DDLTimeoutSeconds = atoi(v)
	case "safety.lock_timeout_ms":
		cfg.Safety.LockTimeoutMs = atoi(v)
	case "safety.index_build_timeout_seconds":
		cfg.Safety.IndexBuildTimeoutSeconds = atoi(v)
	case "safety.reindex_timeout_seconds":
		cfg.Safety.ReindexTimeoutSeconds = atoi(v)
	case "safety.vacuum_timeout_seconds":
		cfg.Safety.VacuumTimeoutSeconds = atoi(v)
	case "safety.vacuum_stall_minutes":
		cfg.Safety.VacuumStallMinutes = atoi(v)
	}
}

func hotReloadLLM(cfg *config.Config, key, v string) {
	switch key {
	case "llm.auth":
		cfg.LLM.Auth = v
	case "llm.enabled":
		cfg.LLM.Enabled = atob(v)
	case "llm.endpoint":
		cfg.LLM.Endpoint = v
	case "llm.api_key":
		cfg.LLM.APIKey = v
	case "llm.model":
		cfg.LLM.Model = v
	case "llm.json_mode":
		cfg.LLM.JSONMode = atob(v)
	case "llm.timeout_seconds":
		cfg.LLM.TimeoutSeconds = atoi(v)
	case "llm.token_budget_daily":
		cfg.LLM.TokenBudgetDaily = atoi(v)
	case "llm.fleet_token_budget_daily":
		cfg.LLM.FleetTokenBudgetDaily = atoi(v)
	case "llm.global_token_budget_daily":
		cfg.LLM.GlobalTokenBudgetDaily = atoi(v)
	case "llm.context_budget_tokens":
		cfg.LLM.ContextBudgetTokens = atoi(v)
	case "llm.optimizer.enabled":
		cfg.LLM.Optimizer.Enabled = atob(v)
	case "llm.optimizer.min_query_calls":
		cfg.LLM.Optimizer.MinQueryCalls = atoi(v)
	case "llm.optimizer.max_new_per_table":
		cfg.LLM.Optimizer.MaxNewPerTable = atoi(v)
	}
}

func hotReloadAdvisor(cfg *config.Config, key, v string) {
	switch key {
	case "advisor.enabled":
		cfg.Advisor.Enabled = atob(v)
	case "advisor.interval_seconds":
		cfg.Advisor.IntervalSeconds = atoi(v)
	}
}

func hotReloadAlerting(cfg *config.Config, key, v string) {
	switch key {
	case "alerting.enabled":
		cfg.Alerting.Enabled = atob(v)
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
	case "retention.sage_size_warning_pct":
		cfg.Retention.SageSizeWarningPct = atoi(v)
	}
}

func hotReloadRCA(cfg *config.Config, key, v string) {
	switch key {
	case "rca.enabled":
		cfg.RCA.Enabled = atob(v)
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
	case "rca.narration_enabled":
		cfg.RCA.NarrationEnabled = atob(v)
	}
}

func hotReloadRunaway(cfg *config.Config, key, v string) {
	switch key {
	case "runaway.enabled":
		cfg.Runaway.Enabled = atob(v)
	}
}

func hotReloadExplain(cfg *config.Config, key, v string) {
	switch key {
	case "explain.enabled":
		cfg.Explain.Enabled = atob(v)
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
		cfg.LogWatch.Enabled = logWatchEnabled(v)
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
		cfg.LogWatch.SlowQueryEnabled = atob(v)
	}
}

func hotReloadSchemaLint(cfg *config.Config, key, v string) {
	switch key {
	case "schema_lint.enabled":
		cfg.SchemaLint.Enabled = atob(v)
	case "schema_lint.scan_interval_minutes":
		cfg.SchemaLint.ScanIntervalMinutes = atoi(v)
	case "schema_lint.min_table_rows":
		cfg.SchemaLint.MinTableRows = atoi(v)
	}
}

func hotReloadMigration(cfg *config.Config, key, v string) {
	switch key {
	case "migration.enabled":
		cfg.Migration.Enabled = atob(v)
	case "migration.mode":
		cfg.Migration.Mode = v
	case "migration.log_detection":
		cfg.Migration.LogDetection = atob(v)
	case "migration.activity_polling":
		cfg.Migration.ActivityPolling = atob(v)
	case "migration.poll_interval_seconds":
		cfg.Migration.PollIntervalSeconds = atoi(v)
	case "migration.ddl_row_threshold":
		cfg.Migration.DDLRowThreshold = atoi(v)
	case "migration.queue_waiters_threshold":
		cfg.Migration.QueueWaitersThreshold = atoi(v)
	}
}

func hotReloadAgentNative(cfg *config.Config, key, v string) {
	switch key {
	case "policy.profile":
		cfg.Policy.Profile = v
	case "value.toil_model_version":
		cfg.Value.ToilModelVersion = atoi(v)
	case "verify.window_minutes":
		cfg.Verify.WindowMinutes = atoi(v)
	case "verify.window_max_minutes":
		cfg.Verify.WindowMaxMinutes = atoi(v)
	case "verify.min_gain_pct":
		cfg.Verify.MinGainPct = atof(v)
	case "verify.regress_pct":
		cfg.Verify.RegressPct = atof(v)
	case "verify.write_impact_pct":
		cfg.Verify.WriteImpactPct = atof(v)
	case "verify.min_samples":
		cfg.Verify.MinSamples = atoi(v)
	case "clone.provider":
		cfg.Clone.Provider = v
	case "clone.dle_endpoint":
		cfg.Clone.DLEEndpoint = v
	case "clone.dle_token":
		cfg.Clone.DLEToken = v
	case "clone.max_clone_age_minutes":
		cfg.Clone.MaxCloneAgeMinutes = atoi(v)
	case "custodian.freeze.red_buffer_pct":
		cfg.Custodian.Freeze.RedBufferPct = atof(v)
	case "custodian.wal.abandon_after_minutes":
		cfg.Custodian.WAL.AbandonAfterMinutes = atoi(v)
	case "custodian.wal.retained_wal_disk_pct_ceiling":
		cfg.Custodian.WAL.RetainedWALDiskPctCeiling = atof(v)
	case "mcp.enabled":
		cfg.MCP.Enabled = atob(v)
	case "mcp.transport":
		cfg.MCP.Transport = v
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
