package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
)

// overridableSpec is the registry contract of a key the configuration API
// may store (config-02b): the api struct tag on the Config field.
func overridableSpec(key string) (config.KeySpec, error) {
	spec, ok := config.LookupKey(key)
	if !ok || !spec.Overridable {
		return config.KeySpec{}, fmt.Errorf("validate: unknown config key %q", key)
	}
	return spec, nil
}

func validateConfigKey(key string) error {
	_, err := overridableSpec(key)
	return err
}

func validateConfigValue(key, value string) error {
	spec, err := overridableSpec(key)
	if err != nil {
		return err
	}
	return validateSpec(spec, value)
}

// ValidateConfigOverride validates a single config override without
// persisting it. Handlers use this to make multi-key saves atomic.
func ValidateConfigOverride(key, value string) error {
	return validateConfigValue(key, value)
}

// validateSpec applies the key's registry range or enum, then the named
// special types' own grammars.
func validateSpec(spec config.KeySpec, value string) error {
	if spec.HasRange {
		return validateNumber(spec, value)
	}
	if err := spec.CheckEnum(value); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	switch spec.Type {
	case "bool":
		return validateBool(spec.Key, value)
	case "bool_auto": // logwatch.enabled also takes auto (rca-09b)
		if _, err := config.ParseLogWatchEnabled(value); err != nil {
			return fmt.Errorf("validate: %w", err)
		}
		return nil
	case "clone_provider":
		return validateAPICloneProvider(value)
	case "maintenance_window":
		return validateMaintenanceWindow(value)
	case "rule_list":
		err := config.ValidateDisabledRules(spec.Key, config.ParseRuleList(value))
		if err != nil {
			return fmt.Errorf("validate: invalid value: %w", err)
		}
	}
	return nil
}

// validateAPICloneProvider refuses lakebase_branch on save: that provider
// needs the clone.lakebase project binding, which is YAML only and
// restart-bound, so the API cannot switch to it.
func validateAPICloneProvider(value string) error {
	if value == "lakebase_branch" {
		return fmt.Errorf("validate: clone.provider lakebase_branch is set in YAML " +
			"with its clone.lakebase project, not through the API")
	}
	return nil
}

func validateNumber(spec config.KeySpec, value string) error {
	var n float64
	if spec.Integer {
		i, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("validate: %s must be an integer, got %q", spec.Key, value)
		}
		n = float64(i)
	} else {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("validate: %s must be a number, got %q", spec.Key, value)
		}
		n = f
	}
	if err := spec.CheckOverride(n); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

// validateMaintenanceWindow applies the policy window grammar so a typo is
// refused on save instead of being stored and silently meaning "never".
func validateMaintenanceWindow(value string) error {
	if err := config.ValidateMaintenanceWindow(value); err != nil {
		return fmt.Errorf("validate: invalid value: %w", err)
	}
	return nil
}

func validateBool(key, value string) error {
	if _, err := config.ParseBool(value); err != nil {
		return fmt.Errorf(
			"validate: %s must be true or false, got %q", key, value)
	}
	return nil
}

func getOldValue(
	ctx context.Context, tx pgx.Tx,
	key string, databaseID int,
) (string, error) {
	var old string
	var err error
	if databaseID == 0 {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`, key,
		).Scan(&old)
	} else {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id = $2`, key, databaseID,
		).Scan(&old)
	}
	if err != nil {
		return "", nil // no existing value
	}
	return old, nil
}

func upsertOverride(
	ctx context.Context, tx pgx.Tx,
	key, value string, databaseID int, userID int,
) error {
	var updatedBy *int // 0 is a system change: no user to name
	if userID > 0 {
		updatedBy = &userID
	}
	if databaseID == 0 {
		_, err := tx.Exec(ctx,
			`/* pg_sage */ INSERT INTO sage.config
				(key, value, database_id, updated_at, updated_by_user_id)
			 VALUES ($1, $2, NULL, now(), $3)
			 ON CONFLICT (key, COALESCE(database_id, 0))
			 DO UPDATE SET value = $2, updated_at = now(),
				updated_by_user_id = $3`,
			key, value, updatedBy)
		return err
	}
	_, err := tx.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config
			(key, value, database_id, updated_at, updated_by_user_id)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = $2, updated_at = now(),
			updated_by_user_id = $4`,
		key, value, databaseID, updatedBy)
	return err
}

func insertAudit(
	ctx context.Context, tx pgx.Tx,
	key, oldValue, newValue string,
	databaseID int, userID int,
) error {
	oldValue, newValue = auditValues(key, oldValue, newValue)
	var dbID *int
	if databaseID > 0 {
		dbID = &databaseID
	}
	var chBy *int
	if userID > 0 {
		chBy = &userID
	}
	// changed_by_actor snapshots the caller's email (apiauth-01): it is
	// sealed with the row, so the trail names the person after their
	// account is deleted. An id with no users row here (fleet mode keeps
	// users on the auth database) leaves it NULL.
	_, err := tx.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config_audit
			(key, old_value, new_value, database_id, changed_by, changed_by_actor)
		 VALUES ($1, $2, $3, $4, $5::int,
			(SELECT u.email FROM sage.users u WHERE u.id = $5::int))`,
		key, nullIfEmpty(oldValue), newValue, dbID, chBy)
	return err
}

const redactedSecret = "[REDACTED]"

// secretKeys are the config keys whose values are credentials: redacted in
// audit and API reads, and sealed at rest when encryption_key is set.
var secretKeys = []string{
	"llm.api_key",
	"alerting.slack_webhook_url",
	"alerting.pagerduty_routing_key",
	"briefing.slack_webhook_url",
	"clone.dle_token",
}

func isSecretConfigKey(key string) bool {
	for _, k := range secretKeys {
		if k == key {
			return true
		}
	}
	return false
}

func secretConfigKeys() []string {
	return append([]string(nil), secretKeys...)
}

func auditValues(key, oldValue, newValue string) (string, string) {
	if !isSecretConfigKey(key) {
		return oldValue, newValue
	}
	if oldValue != "" {
		oldValue = redactedSecret
	}
	if newValue != "" {
		newValue = redactedSecret
	}
	return oldValue, newValue
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanOverrideRows(
	rows pgx.Rows,
) ([]ConfigOverride, error) {
	var results []ConfigOverride
	for rows.Next() {
		var o ConfigOverride
		err := rows.Scan(
			&o.Key, &o.Value, &o.DatabaseID,
			&o.UpdatedAt, &o.UpdatedBy)
		if err != nil {
			return nil, fmt.Errorf("scanning override: %w", err)
		}
		results = append(results, o)
	}
	return results, rows.Err()
}

func scanAuditRows(rows pgx.Rows) ([]ConfigAuditEntry, error) {
	var results []ConfigAuditEntry
	for rows.Next() {
		var e ConfigAuditEntry
		err := rows.Scan(
			&e.ID, &e.Key, &e.OldValue, &e.NewValue,
			&e.DatabaseID, &e.ChangedBy, &e.ChangedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning audit: %w", err)
		}
		e.OldValue, e.NewValue = auditValues(
			e.Key, e.OldValue, e.NewValue,
		)
		results = append(results, e)
	}
	return results, rows.Err()
}

// SourceFileDefault labels a value no persisted override sets: the config
// file, the environment, a flag or the built-in default (config-03). The
// presence set cannot say which, so the badge does not pretend to.
const SourceFileDefault = "file/default"

// configToMap converts a Config struct into a flat map with source
// indicators.
func configToMap(cfg *config.Config) map[string]any {
	m := make(map[string]any, 70)
	addField(m, "collector.interval_seconds",
		cfg.Collector.IntervalSeconds, SourceFileDefault)
	addField(m, "collector.batch_size",
		cfg.Collector.BatchSize, SourceFileDefault)
	addField(m, "collector.max_queries",
		cfg.Collector.MaxQueries, SourceFileDefault)
	addAnalyzerFields(m, &cfg.Analyzer)
	addTrustFields(m, &cfg.Trust)
	addSafetyFields(m, &cfg.Safety)
	addLLMFields(m, &cfg.LLM)
	addAdvisorFields(m, &cfg.Advisor)
	addAlertingFields(m, &cfg.Alerting)
	addRetentionFields(m, &cfg.Retention)
	addRCAFields(m, &cfg.RCA)
	addRunawayFields(m, &cfg.Runaway)
	addExplainFields(m, &cfg.Explain)
	addLogWatchFields(m, &cfg.LogWatch)
	addSchemaLintFields(m, &cfg.SchemaLint)
	addMigrationFields(m, &cfg.Migration)
	addAgentNativeFields(m, cfg)
	return m
}

// ConfigReadModel returns the effective configuration in the same redacted,
// source-annotated shape used by the persistent configuration API. It does
// not consult or mutate the override store.
func ConfigReadModel(cfg *config.Config) map[string]any {
	return configToMap(cfg)
}

func addField(m map[string]any, key string, val any, src string) {
	m[key] = map[string]any{"value": val, "source": src}
}

func addAnalyzerFields(m map[string]any, a *config.AnalyzerConfig) {
	addField(m, "analyzer.interval_seconds",
		a.IntervalSeconds, SourceFileDefault)
	addField(m, "analyzer.slow_query_threshold_ms",
		a.SlowQueryThresholdMs, SourceFileDefault)
	addField(m, "analyzer.self_cost_budget_ms",
		a.SelfCostBudgetMs, SourceFileDefault)
	addField(m, "analyzer.inactive_slot_min_minutes",
		a.InactiveSlotMinMinutes, SourceFileDefault)
	addField(m, "analyzer.seq_scan_min_rows",
		a.SeqScanMinRows, SourceFileDefault)
	addField(m, "analyzer.unused_index_window_days",
		a.UnusedIndexWindowDays, SourceFileDefault)
	addField(m, "analyzer.index_bloat_threshold_pct",
		a.IndexBloatThresholdPct, SourceFileDefault)
	addField(m, "analyzer.table_bloat_dead_tuple_pct",
		a.TableBloatDeadTuplePct, SourceFileDefault)
	addField(m, "analyzer.autovacuum_tune_min_rows",
		a.AutovacuumTuneMinRows, SourceFileDefault)
	addField(m, "analyzer.analyze_stale_min_rows",
		a.AnalyzeStaleMinRows, SourceFileDefault)
	addField(m, "analyzer.analyze_stale_days",
		a.AnalyzeStaleDays, SourceFileDefault)
	addField(m, "analyzer.wraparound_freeze_share",
		a.WraparoundFreezeShare, SourceFileDefault)
	addField(m, "analyzer.regression_threshold_pct",
		a.RegressionThresholdPct, SourceFileDefault)
	addField(m, "analyzer.cache_hit_ratio_warning",
		a.CacheHitRatioWarning, SourceFileDefault)
	addField(m, "analyzer.disabled_rules",
		strings.Join(a.DisabledRules, ","), SourceFileDefault)
	addField(m, "analyzer.slow_slot_retained_bytes",
		a.SlowSlotRetainedBytes, SourceFileDefault)
	addField(m, "analyzer.lock_chain.enabled",
		a.LockChain.Enabled, SourceFileDefault)
	addField(m, "analyzer.lock_chain.min_blocked_threshold",
		a.LockChain.MinBlockedThreshold, SourceFileDefault)
	addField(m, "analyzer.lock_chain.critical_blocked_threshold",
		a.LockChain.CriticalBlockedThreshold, SourceFileDefault)
	addField(m, "analyzer.lock_chain.idle_in_tx_terminate_minutes",
		a.LockChain.IdleInTxTerminateMinutes, SourceFileDefault)
	addField(m, "analyzer.lock_chain.active_query_cancel_minutes",
		a.LockChain.ActiveQueryCancelMinutes, SourceFileDefault)
}

func addTrustFields(m map[string]any, t *config.TrustConfig) {
	addField(m, "trust.level", t.Level, SourceFileDefault)
	addField(m, "trust.tier3_safe", t.Tier3Safe, SourceFileDefault)
	addField(m, "trust.tier3_moderate", t.Tier3Moderate, SourceFileDefault)
	addField(m, "trust.tier3_high_risk", t.Tier3HighRisk, SourceFileDefault)
	addField(m, "trust.maintenance_window",
		t.MaintenanceWindow, SourceFileDefault)
	addField(m, "trust.rollback_threshold_pct",
		t.RollbackThresholdPct, SourceFileDefault)
	addField(m, "trust.rollback_window_minutes",
		t.RollbackWindowMinutes, SourceFileDefault)
	addField(m, "trust.rollback_cooldown_days",
		t.RollbackCooldownDays, SourceFileDefault)
	addField(m, "trust.cascade_cooldown_cycles",
		t.CascadeCooldownCycles, SourceFileDefault)
}

func addSafetyFields(m map[string]any, s *config.SafetyConfig) {
	addField(m, "safety.cpu_ceiling_pct", s.CPUCeilingPct, SourceFileDefault)
	addField(m, "safety.host_cpu_ceiling_pct", s.EffectiveHostCPUCeilingPct(), SourceFileDefault)
	addField(m, "safety.query_timeout_ms", s.QueryTimeoutMs, SourceFileDefault)
	addField(m, "safety.ddl_timeout_seconds",
		s.DDLTimeoutSeconds, SourceFileDefault)
	addField(m, "safety.lock_timeout_ms", s.LockTimeoutMs, SourceFileDefault)
	addField(m, "safety.index_build_timeout_seconds",
		s.IndexBuildTimeoutSeconds, SourceFileDefault)
	addField(m, "safety.reindex_timeout_seconds",
		s.ReindexTimeoutSeconds, SourceFileDefault)
	addField(m, "safety.vacuum_timeout_seconds",
		s.VacuumTimeoutSeconds, SourceFileDefault)
	addField(m, "safety.vacuum_stall_minutes",
		s.VacuumStallMinutes, SourceFileDefault)
}

func addLLMFields(m map[string]any, l *config.LLMConfig) {
	addField(m, "llm.enabled", l.Enabled, SourceFileDefault)
	addField(m, "llm.auth", l.Auth, SourceFileDefault)
	addField(m, "llm.endpoint", l.Endpoint, SourceFileDefault)
	addField(m, "llm.api_key", maskSecret(l.APIKey), SourceFileDefault)
	addField(m, "llm.model", l.Model, SourceFileDefault)
	addField(m, "llm.json_mode", l.JSONMode, SourceFileDefault)
	addField(m, "llm.timeout_seconds", l.TimeoutSeconds, SourceFileDefault)
	addField(m, "llm.token_budget_daily",
		l.TokenBudgetDaily, SourceFileDefault)
	addField(m, "llm.fleet_token_budget_daily",
		l.FleetTokenBudgetDaily, SourceFileDefault)
	addField(m, "llm.global_token_budget_daily",
		l.GlobalTokenBudgetDaily, SourceFileDefault)
	addField(m, "llm.context_budget_tokens",
		l.ContextBudgetTokens, SourceFileDefault)
	addField(m, "llm.optimizer.enabled",
		l.Optimizer.Enabled, SourceFileDefault)
	addField(m, "llm.optimizer.min_query_calls",
		l.Optimizer.MinQueryCalls, SourceFileDefault)
	addField(m, "llm.optimizer.max_new_per_table",
		l.Optimizer.MaxNewPerTable, SourceFileDefault)
}

func addAdvisorFields(m map[string]any, a *config.AdvisorConfig) {
	addField(m, "advisor.enabled", a.Enabled, SourceFileDefault)
	addField(m, "advisor.interval_seconds",
		a.IntervalSeconds, SourceFileDefault)
}

func addAlertingFields(
	m map[string]any, a *config.AlertingConfig,
) {
	addField(m, "alerting.enabled", a.Enabled, SourceFileDefault)
	addField(m, "alerting.slack_webhook_url",
		maskSecret(a.SlackWebhookURL), SourceFileDefault)
	addField(m, "alerting.pagerduty_routing_key",
		maskSecret(a.PagerDutyRoutingKey), SourceFileDefault)
	addField(m, "alerting.check_interval_seconds",
		a.CheckIntervalSeconds, SourceFileDefault)
	addField(m, "alerting.cooldown_minutes",
		a.CooldownMinutes, SourceFileDefault)
	addField(m, "alerting.quiet_hours_start",
		a.QuietHoursStart, SourceFileDefault)
	addField(m, "alerting.quiet_hours_end",
		a.QuietHoursEnd, SourceFileDefault)
}

func addRetentionFields(
	m map[string]any, r *config.RetentionConfig,
) {
	addField(m, "retention.snapshots_days",
		r.SnapshotsDays, SourceFileDefault)
	addField(m, "retention.findings_days",
		r.FindingsDays, SourceFileDefault)
	addField(m, "retention.actions_days",
		r.ActionsDays, SourceFileDefault)
	addField(m, "retention.explains_days",
		r.ExplainsDays, SourceFileDefault)
	addField(m, "retention.sage_size_warning_pct",
		r.SageSizeWarningPct, SourceFileDefault)
}

func addRCAFields(m map[string]any, r *config.RCAConfig) {
	addField(m, "rca.enabled", r.Enabled, SourceFileDefault)
	addField(m, "rca.llm_correlation_threshold",
		r.LLMCorrelationThreshold, SourceFileDefault)
	addField(m, "rca.dedup_window_minutes",
		r.DedupWindowMinutes, SourceFileDefault)
	addField(m, "rca.escalation_cycles",
		r.EscalationCycles, SourceFileDefault)
	addField(m, "rca.resolution_cycles",
		r.ResolutionCycles, SourceFileDefault)
	addField(m, "rca.connection_saturation_pct",
		r.ConnectionSaturationPct, SourceFileDefault)
	addField(m, "rca.replication_lag_threshold_seconds",
		r.ReplicationLagThresholdS, SourceFileDefault)
	addField(m, "rca.wal_spike_multiplier",
		r.WALSpikeMultiplier, SourceFileDefault)
	addField(m, "rca.narration_enabled", r.NarrationEnabled, SourceFileDefault)
}

func addRunawayFields(m map[string]any, r *config.RunawayConfig) {
	addField(m, "runaway.enabled", r.Enabled, SourceFileDefault)
}

func addExplainFields(m map[string]any, e *config.ExplainConfig) {
	addField(m, "explain.enabled", e.Enabled, SourceFileDefault)
	addField(m, "explain.timeout_ms", e.TimeoutMs, SourceFileDefault)
	addField(m, "explain.cache_ttl_minutes",
		e.CacheTTLMinutes, SourceFileDefault)
	addField(m, "explain.max_tokens", e.MaxTokens, SourceFileDefault)
}

func addLogWatchFields(
	m map[string]any, lw *config.LogWatchConfig,
) {
	addField(m, "logwatch.enabled", coerceValue("logwatch.enabled", lw.Setting()),
		SourceFileDefault)
	addField(m, "logwatch.log_directory",
		lw.LogDirectory, SourceFileDefault)
	addField(m, "logwatch.format", lw.Format, SourceFileDefault)
	addField(m, "logwatch.poll_interval_ms",
		lw.PollIntervalMs, SourceFileDefault)
	addField(m, "logwatch.dedup_window_seconds",
		lw.DedupWindowS, SourceFileDefault)
	addField(m, "logwatch.max_line_len_bytes",
		lw.MaxLineLenBytes, SourceFileDefault)
	addField(m, "logwatch.temp_file_min_bytes",
		lw.TempFileMinBytes, SourceFileDefault)
	addField(m, "logwatch.max_lines_per_cycle",
		lw.MaxLinesPerCycle, SourceFileDefault)
	addField(m, "logwatch.exclude_applications",
		strings.Join(lw.ExcludeApplications, ","), SourceFileDefault)
	addField(m, "logwatch.slow_query_enabled",
		lw.SlowQueryEnabled, SourceFileDefault)
}

func addSchemaLintFields(
	m map[string]any, sl *config.SchemaLintConfig,
) {
	addField(m, "schema_lint.enabled", sl.Enabled, SourceFileDefault)
	addField(m, "schema_lint.scan_interval_minutes",
		sl.ScanIntervalMinutes, SourceFileDefault)
	addField(m, "schema_lint.min_table_rows",
		sl.MinTableRows, SourceFileDefault)
}

func addMigrationFields(
	m map[string]any, mg *config.MigrationConfig,
) {
	addField(m, "migration.enabled", mg.Enabled, SourceFileDefault)
	addField(m, "migration.mode", mg.Mode, SourceFileDefault)
	addField(m, "migration.log_detection",
		mg.LogDetection, SourceFileDefault)
	addField(m, "migration.activity_polling",
		mg.ActivityPolling, SourceFileDefault)
	addField(m, "migration.poll_interval_seconds",
		mg.PollIntervalSeconds, SourceFileDefault)
	addField(m, "migration.ddl_row_threshold",
		mg.DDLRowThreshold, SourceFileDefault)
	addField(m, "migration.queue_waiters_threshold",
		mg.QueueWaitersThreshold, SourceFileDefault)
}

func addAgentNativeFields(m map[string]any, cfg *config.Config) {
	addField(m, "policy.profile", cfg.Policy.Profile, SourceFileDefault)
	addField(m, "value.toil_model_version", cfg.Value.ToilModelVersion, SourceFileDefault)
	addField(m, "verify.window_minutes", cfg.Verify.WindowMinutes, SourceFileDefault)
	addField(m, "verify.window_max_minutes", cfg.Verify.WindowMaxMinutes, SourceFileDefault)
	addField(m, "verify.min_gain_pct", cfg.Verify.MinGainPct, SourceFileDefault)
	addField(m, "verify.regress_pct", cfg.Verify.RegressPct, SourceFileDefault)
	addField(m, "verify.write_impact_pct", cfg.Verify.WriteImpactPct, SourceFileDefault)
	addField(m, "verify.min_samples", cfg.Verify.MinSamples, SourceFileDefault)
	addField(m, "clone.provider", cfg.Clone.Provider, SourceFileDefault)
	addField(m, "clone.dle_endpoint", cfg.Clone.DLEEndpoint, SourceFileDefault)
	addField(m, "clone.dle_token", maskSecret(cfg.Clone.DLEToken), SourceFileDefault)
	addField(m, "clone.max_clone_age_minutes", cfg.Clone.MaxCloneAgeMinutes,
		SourceFileDefault)
	addField(m, "custodian.freeze.red_buffer_pct", cfg.Custodian.Freeze.RedBufferPct,
		SourceFileDefault)
	addField(m, "custodian.wal.abandon_after_minutes",
		cfg.Custodian.WAL.AbandonAfterMinutes, SourceFileDefault)
	addField(m, "custodian.wal.retained_wal_disk_pct_ceiling",
		cfg.Custodian.WAL.RetainedWALDiskPctCeiling, SourceFileDefault)
	addField(m, "mcp.enabled", cfg.MCP.Enabled, SourceFileDefault)
	addField(m, "mcp.transport", cfg.MCP.Transport, SourceFileDefault)
}

func maskSecret(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

// applyOverrides merges overrides into the flat config map,
// updating the source indicator.
func applyOverrides(
	m map[string]any, overrides []ConfigOverride, source string,
) {
	for _, o := range overrides {
		replayed, ok, _ := ReplayOverride(o.Key, o.Value)
		if !ok {
			continue // the runtime skips it too (config-02)
		}
		if existing, ok := m[o.Key]; ok {
			if em, ok := existing.(map[string]any); ok {
				value := coerceValue(o.Key, replayed)
				if isSecretConfigKey(o.Key) {
					value = maskSecret(o.Value)
				}
				em["value"] = value
				em["source"] = source
			}
		}
	}
}

// coerceValue converts a string value back to its expected Go type
// based on the config key.
func coerceValue(key, value string) any {
	spec, _ := config.LookupKey(key)
	switch {
	case spec.HasRange && spec.Integer:
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	case spec.HasRange:
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	case spec.Type == "bool", spec.Type == "bool_auto":
		if b, err := config.ParseBool(value); err == nil {
			return b
		}
	}
	return value
}

// configKeyTypes maps every overridable key to its API validation type,
// read from the config registry.
func configKeyTypes() map[string]string {
	out := make(map[string]string)
	for _, spec := range config.Registry() {
		if spec.Overridable {
			out[spec.Key] = spec.Type
		}
	}
	return out
}

// AllowedConfigKeysSnapshot returns a fresh copy of the overridable keys
// (key -> validation type). Exported so cross-package consistency tests
// and handlers can iterate the registry without duplicating it.
func AllowedConfigKeysSnapshot() map[string]string {
	return configKeyTypes()
}
