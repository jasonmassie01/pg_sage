package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
)

// allowedConfigKeys maps dot-notation keys to their value type.
var allowedConfigKeys = map[string]string{
	"collector.interval_seconds":     "int_min5",
	"collector.batch_size":           "int_pos",
	"collector.max_queries":          "int_pos",
	"analyzer.interval_seconds":      "int_min5",
	"analyzer.slow_query_threshold_ms": "int_nonneg",
	"analyzer.seq_scan_min_rows":     "int_pos",
	"analyzer.unused_index_window_days": "int_pos",
	"analyzer.index_bloat_threshold_pct": "pct",
	"analyzer.table_bloat_dead_tuple_pct": "pct",
	"analyzer.regression_threshold_pct": "pct",
	"analyzer.cache_hit_ratio_warning": "float01",
	"trust.level":                    "trust_level",
	"trust.tier3_safe":               "bool",
	"trust.tier3_moderate":           "bool",
	"trust.tier3_high_risk":          "bool",
	"trust.maintenance_window":       "string",
	"trust.rollback_threshold_pct":   "pct",
	"trust.rollback_window_minutes":  "int_pos",
	"trust.rollback_cooldown_days":   "int_pos",
	"trust.cascade_cooldown_cycles":  "int_pos",
	"safety.cpu_ceiling_pct":         "pct1_100",
	"safety.query_timeout_ms":        "int_pos",
	"safety.ddl_timeout_seconds":     "int_pos",
	"safety.lock_timeout_ms":         "int_pos",
	"execution_mode":                 "exec_mode",
	"llm.enabled":                    "bool",
	"llm.endpoint":                   "string",
	"llm.api_key":                    "string",
	"llm.model":                      "string",
	"llm.timeout_seconds":            "int_pos",
	"llm.token_budget_daily":         "int_pos",
	"llm.context_budget_tokens":      "int_pos",
	"alerting.enabled":               "bool",
	"alerting.slack_webhook_url":     "string",
	"alerting.pagerduty_routing_key": "string",
	"alerting.check_interval_seconds": "int_min5",
	"alerting.cooldown_minutes":      "int_pos",
	"alerting.quiet_hours_start":     "string",
	"alerting.quiet_hours_end":       "string",
	"retention.snapshots_days":       "int_pos",
	"retention.findings_days":        "int_pos",
	"retention.actions_days":         "int_pos",
	"retention.explains_days":        "int_pos",
}

func validateConfigKey(key string) error {
	if _, ok := allowedConfigKeys[key]; !ok {
		return fmt.Errorf("validate: unknown config key %q", key)
	}
	return nil
}

func validateConfigValue(key, value string) error {
	vtype, ok := allowedConfigKeys[key]
	if !ok {
		return fmt.Errorf("validate: unknown config key %q", key)
	}
	return validateByType(vtype, key, value)
}

func validateByType(vtype, key, value string) error {
	switch vtype {
	case "int_pos":
		return validateIntRange(key, value, 1, 1<<30)
	case "int_nonneg":
		return validateIntRange(key, value, 0, 1<<30)
	case "int_min5":
		return validateIntRange(key, value, 5, 1<<30)
	case "pct":
		return validateIntRange(key, value, 0, 100)
	case "pct1_100":
		return validateIntRange(key, value, 1, 100)
	case "float01":
		return validateFloatRange(key, value, 0, 1)
	case "bool":
		return validateBool(key, value)
	case "trust_level":
		return validateEnum(key, value, validTrustLevels)
	case "exec_mode":
		return validateEnum(key, value, validExecutionModes)
	case "string":
		return nil
	default:
		return nil
	}
}

func validateIntRange(
	key, value string, min, max int,
) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf(
			"validate: %s must be an integer, got %q", key, value)
	}
	if n < min || n > max {
		return fmt.Errorf(
			"validate: %s must be %d-%d, got %d", key, min, max, n)
	}
	return nil
}

func validateFloatRange(
	key, value string, min, max float64,
) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf(
			"validate: %s must be a number, got %q", key, value)
	}
	if f < min || f > max {
		return fmt.Errorf(
			"validate: %s must be %g-%g, got %g", key, min, max, f)
	}
	return nil
}

func validateBool(key, value string) error {
	v := strings.ToLower(value)
	if v != "true" && v != "false" {
		return fmt.Errorf(
			"validate: %s must be true or false, got %q", key, value)
	}
	return nil
}

func validateEnum(
	key, value string, allowed map[string]bool,
) error {
	if !allowed[value] {
		keys := make([]string, 0, len(allowed))
		for k := range allowed {
			keys = append(keys, k)
		}
		return fmt.Errorf(
			"validate: %s must be one of %s, got %q",
			key, strings.Join(keys, ", "), value)
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
			`SELECT value FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`, key,
		).Scan(&old)
	} else {
		err = tx.QueryRow(ctx,
			`SELECT value FROM sage.config
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
	if databaseID == 0 {
		_, err := tx.Exec(ctx,
			`INSERT INTO sage.config
				(key, value, database_id, updated_at, updated_by_user_id)
			 VALUES ($1, $2, NULL, now(), $3)
			 ON CONFLICT (key, COALESCE(database_id, 0))
			 DO UPDATE SET value = $2, updated_at = now(),
				updated_by_user_id = $3`,
			key, value, userID)
		return err
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO sage.config
			(key, value, database_id, updated_at, updated_by_user_id)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = $2, updated_at = now(),
			updated_by_user_id = $4`,
		key, value, databaseID, userID)
	return err
}

func insertAudit(
	ctx context.Context, tx pgx.Tx,
	key, oldValue, newValue string,
	databaseID int, userID int,
) error {
	var dbID *int
	if databaseID > 0 {
		dbID = &databaseID
	}
	var chBy *int
	if userID > 0 {
		chBy = &userID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO sage.config_audit
			(key, old_value, new_value, database_id, changed_by)
		 VALUES ($1, $2, $3, $4, $5)`,
		key, nullIfEmpty(oldValue), newValue, dbID, chBy)
	return err
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
		results = append(results, e)
	}
	return results, rows.Err()
}

// configToMap converts a Config struct into a flat map with source
// indicators.
func configToMap(cfg *config.Config) map[string]any {
	m := make(map[string]any, 60)
	addField(m, "collector.interval_seconds",
		cfg.Collector.IntervalSeconds, "yaml")
	addField(m, "collector.batch_size",
		cfg.Collector.BatchSize, "yaml")
	addField(m, "collector.max_queries",
		cfg.Collector.MaxQueries, "yaml")
	addAnalyzerFields(m, &cfg.Analyzer)
	addTrustFields(m, &cfg.Trust)
	addSafetyFields(m, &cfg.Safety)
	addLLMFields(m, &cfg.LLM)
	addAlertingFields(m, &cfg.Alerting)
	addRetentionFields(m, &cfg.Retention)
	return m
}

func addField(m map[string]any, key string, val any, src string) {
	m[key] = map[string]any{"value": val, "source": src}
}

func addAnalyzerFields(m map[string]any, a *config.AnalyzerConfig) {
	addField(m, "analyzer.interval_seconds",
		a.IntervalSeconds, "yaml")
	addField(m, "analyzer.slow_query_threshold_ms",
		a.SlowQueryThresholdMs, "yaml")
	addField(m, "analyzer.seq_scan_min_rows",
		a.SeqScanMinRows, "yaml")
	addField(m, "analyzer.unused_index_window_days",
		a.UnusedIndexWindowDays, "yaml")
	addField(m, "analyzer.index_bloat_threshold_pct",
		a.IndexBloatThresholdPct, "yaml")
	addField(m, "analyzer.table_bloat_dead_tuple_pct",
		a.TableBloatDeadTuplePct, "yaml")
	addField(m, "analyzer.regression_threshold_pct",
		a.RegressionThresholdPct, "yaml")
	addField(m, "analyzer.cache_hit_ratio_warning",
		a.CacheHitRatioWarning, "yaml")
}

func addTrustFields(m map[string]any, t *config.TrustConfig) {
	addField(m, "trust.level", t.Level, "yaml")
	addField(m, "trust.tier3_safe", t.Tier3Safe, "yaml")
	addField(m, "trust.tier3_moderate", t.Tier3Moderate, "yaml")
	addField(m, "trust.tier3_high_risk", t.Tier3HighRisk, "yaml")
	addField(m, "trust.maintenance_window",
		t.MaintenanceWindow, "yaml")
	addField(m, "trust.rollback_threshold_pct",
		t.RollbackThresholdPct, "yaml")
	addField(m, "trust.rollback_window_minutes",
		t.RollbackWindowMinutes, "yaml")
	addField(m, "trust.rollback_cooldown_days",
		t.RollbackCooldownDays, "yaml")
	addField(m, "trust.cascade_cooldown_cycles",
		t.CascadeCooldownCycles, "yaml")
}

func addSafetyFields(m map[string]any, s *config.SafetyConfig) {
	addField(m, "safety.cpu_ceiling_pct", s.CPUCeilingPct, "yaml")
	addField(m, "safety.query_timeout_ms", s.QueryTimeoutMs, "yaml")
	addField(m, "safety.ddl_timeout_seconds",
		s.DDLTimeoutSeconds, "yaml")
	addField(m, "safety.lock_timeout_ms", s.LockTimeoutMs, "yaml")
}

func addLLMFields(m map[string]any, l *config.LLMConfig) {
	addField(m, "llm.enabled", l.Enabled, "yaml")
	addField(m, "llm.endpoint", l.Endpoint, "yaml")
	addField(m, "llm.api_key", maskSecret(l.APIKey), "yaml")
	addField(m, "llm.model", l.Model, "yaml")
	addField(m, "llm.timeout_seconds", l.TimeoutSeconds, "yaml")
	addField(m, "llm.token_budget_daily",
		l.TokenBudgetDaily, "yaml")
	addField(m, "llm.context_budget_tokens",
		l.ContextBudgetTokens, "yaml")
}

func addAlertingFields(
	m map[string]any, a *config.AlertingConfig,
) {
	addField(m, "alerting.enabled", a.Enabled, "yaml")
	addField(m, "alerting.slack_webhook_url",
		maskSecret(a.SlackWebhookURL), "yaml")
	addField(m, "alerting.pagerduty_routing_key",
		maskSecret(a.PagerDutyRoutingKey), "yaml")
	addField(m, "alerting.check_interval_seconds",
		a.CheckIntervalSeconds, "yaml")
	addField(m, "alerting.cooldown_minutes",
		a.CooldownMinutes, "yaml")
	addField(m, "alerting.quiet_hours_start",
		a.QuietHoursStart, "yaml")
	addField(m, "alerting.quiet_hours_end",
		a.QuietHoursEnd, "yaml")
}

func addRetentionFields(
	m map[string]any, r *config.RetentionConfig,
) {
	addField(m, "retention.snapshots_days",
		r.SnapshotsDays, "yaml")
	addField(m, "retention.findings_days",
		r.FindingsDays, "yaml")
	addField(m, "retention.actions_days",
		r.ActionsDays, "yaml")
	addField(m, "retention.explains_days",
		r.ExplainsDays, "yaml")
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
		if existing, ok := m[o.Key]; ok {
			if em, ok := existing.(map[string]any); ok {
				em["value"] = coerceValue(o.Key, o.Value)
				em["source"] = source
			}
		}
	}
}

// coerceValue converts a string value back to its expected Go type
// based on the config key.
func coerceValue(key, value string) any {
	vtype := allowedConfigKeys[key]
	switch vtype {
	case "int_pos", "int_nonneg", "int_min5", "pct", "pct1_100":
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
	case "float01":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	case "bool":
		return strings.ToLower(value) == "true"
	}
	return value
}
