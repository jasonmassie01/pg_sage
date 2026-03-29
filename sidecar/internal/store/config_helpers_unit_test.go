package store

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestConfigToMap_AllKeysPresent(t *testing.T) {
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 30,
			BatchSize:       100,
			MaxQueries:      500,
		},
		Analyzer: config.AnalyzerConfig{
			IntervalSeconds:        60,
			SlowQueryThresholdMs:   1000,
			SeqScanMinRows:         10000,
			UnusedIndexWindowDays:  7,
			IndexBloatThresholdPct: 30,
			TableBloatDeadTuplePct: 10,
			RegressionThresholdPct: 25,
			CacheHitRatioWarning:   0.95,
		},
		Trust: config.TrustConfig{
			Level:                 "advisory",
			Tier3Safe:             true,
			Tier3Moderate:         false,
			Tier3HighRisk:         false,
			MaintenanceWindow:     "02:00-06:00",
			RollbackThresholdPct:  20,
			RollbackWindowMinutes: 30,
			RollbackCooldownDays:  3,
			CascadeCooldownCycles: 2,
		},
		Safety: config.SafetyConfig{
			CPUCeilingPct:     80,
			QueryTimeoutMs:    5000,
			DDLTimeoutSeconds: 300,
			LockTimeoutMs:     10000,
		},
		LLM: config.LLMConfig{
			Enabled:             true,
			Endpoint:            "https://api.example.com",
			APIKey:              "sk-test-1234567890",
			Model:               "gpt-4",
			TimeoutSeconds:      30,
			TokenBudgetDaily:    100000,
			ContextBudgetTokens: 4096,
		},
		Alerting: config.AlertingConfig{
			Enabled:              true,
			SlackWebhookURL:      "https://hooks.slack.com/services/T00/B00/xxx",
			PagerDutyRoutingKey:  "pd-key-12345678",
			CheckIntervalSeconds: 60,
			CooldownMinutes:      15,
			QuietHoursStart:      "23:00",
			QuietHoursEnd:        "07:00",
		},
		Retention: config.RetentionConfig{
			SnapshotsDays: 30,
			FindingsDays:  90,
			ActionsDays:   365,
			ExplainsDays:  14,
		},
	}

	m := configToMap(cfg)

	// Every key in allowedConfigKeys should exist in the map.
	// Some keys might not be mapped (execution_mode is set at a
	// different level), but all section-specific keys should be present.
	expectedKeys := []string{
		// Collector
		"collector.interval_seconds",
		"collector.batch_size",
		"collector.max_queries",
		// Analyzer
		"analyzer.interval_seconds",
		"analyzer.slow_query_threshold_ms",
		"analyzer.seq_scan_min_rows",
		"analyzer.unused_index_window_days",
		"analyzer.index_bloat_threshold_pct",
		"analyzer.table_bloat_dead_tuple_pct",
		"analyzer.regression_threshold_pct",
		"analyzer.cache_hit_ratio_warning",
		// Trust
		"trust.level",
		"trust.tier3_safe",
		"trust.tier3_moderate",
		"trust.tier3_high_risk",
		"trust.maintenance_window",
		"trust.rollback_threshold_pct",
		"trust.rollback_window_minutes",
		"trust.rollback_cooldown_days",
		"trust.cascade_cooldown_cycles",
		// Safety
		"safety.cpu_ceiling_pct",
		"safety.query_timeout_ms",
		"safety.ddl_timeout_seconds",
		"safety.lock_timeout_ms",
		// LLM
		"llm.enabled",
		"llm.endpoint",
		"llm.api_key",
		"llm.model",
		"llm.timeout_seconds",
		"llm.token_budget_daily",
		"llm.context_budget_tokens",
		// Alerting
		"alerting.enabled",
		"alerting.slack_webhook_url",
		"alerting.pagerduty_routing_key",
		"alerting.check_interval_seconds",
		"alerting.cooldown_minutes",
		"alerting.quiet_hours_start",
		"alerting.quiet_hours_end",
		// Retention
		"retention.snapshots_days",
		"retention.findings_days",
		"retention.actions_days",
		"retention.explains_days",
	}

	for _, key := range expectedKeys {
		entry, ok := m[key]
		if !ok {
			t.Errorf("configToMap missing key %q", key)
			continue
		}
		em, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("key %q: expected map[string]any, got %T", key, entry)
			continue
		}
		if _, hasValue := em["value"]; !hasValue {
			t.Errorf("key %q: missing 'value' in entry", key)
		}
		if src, hasSrc := em["source"]; !hasSrc {
			t.Errorf("key %q: missing 'source' in entry", key)
		} else if src != "yaml" {
			t.Errorf("key %q: source = %q, want %q", key, src, "yaml")
		}
	}
}

func TestConfigToMap_FieldValues(t *testing.T) {
	cfg := &config.Config{
		Collector: config.CollectorConfig{
			IntervalSeconds: 42,
			BatchSize:       200,
			MaxQueries:      999,
		},
	}

	m := configToMap(cfg)

	tests := []struct {
		key  string
		want any
	}{
		{"collector.interval_seconds", 42},
		{"collector.batch_size", 200},
		{"collector.max_queries", 999},
	}

	for _, tt := range tests {
		entry := m[tt.key].(map[string]any)
		got := entry["value"]
		if got != tt.want {
			t.Errorf("configToMap[%q] = %v (%T), want %v (%T)",
				tt.key, got, got, tt.want, tt.want)
		}
	}
}

func TestConfigToMap_ZeroValueConfig(t *testing.T) {
	cfg := &config.Config{}
	m := configToMap(cfg)

	// Should still produce entries for all keys, even with zero values.
	if len(m) == 0 {
		t.Fatal("configToMap returned empty map for zero-value config")
	}

	// Verify a zero-value int is stored as 0, not omitted.
	entry, ok := m["collector.interval_seconds"]
	if !ok {
		t.Fatal("missing collector.interval_seconds for zero config")
	}
	em := entry.(map[string]any)
	if em["value"] != 0 {
		t.Errorf("zero config collector.interval_seconds = %v, want 0",
			em["value"])
	}
}

func TestConfigToMap_MasksSecrets(t *testing.T) {
	cfg := &config.Config{
		LLM: config.LLMConfig{
			APIKey: "sk-secret-key-12345678",
		},
		Alerting: config.AlertingConfig{
			SlackWebhookURL:     "https://hooks.slack.com/T00/B00/xxxx",
			PagerDutyRoutingKey: "pd-routing-key-abcd",
		},
	}

	m := configToMap(cfg)

	// LLM API key should be masked except last 4 chars.
	llmKey := m["llm.api_key"].(map[string]any)["value"].(string)
	if llmKey == "sk-secret-key-12345678" {
		t.Error("llm.api_key was not masked")
	}
	wantSuffix := "5678"
	if len(llmKey) < 4 || llmKey[len(llmKey)-4:] != wantSuffix {
		t.Errorf("llm.api_key masked value %q does not end with %q",
			llmKey, wantSuffix)
	}

	// Slack webhook should be masked.
	slackVal := m["alerting.slack_webhook_url"].(map[string]any)["value"].(string)
	if slackVal == "https://hooks.slack.com/T00/B00/xxxx" {
		t.Error("alerting.slack_webhook_url was not masked")
	}

	// PagerDuty should be masked.
	pdVal := m["alerting.pagerduty_routing_key"].(map[string]any)["value"].(string)
	if pdVal == "pd-routing-key-abcd" {
		t.Error("alerting.pagerduty_routing_key was not masked")
	}
}

func TestAddField(t *testing.T) {
	m := make(map[string]any)
	addField(m, "test.key", 42, "yaml")

	entry, ok := m["test.key"]
	if !ok {
		t.Fatal("addField did not set the key")
	}

	em, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("entry is %T, want map[string]any", entry)
	}

	if em["value"] != 42 {
		t.Errorf("value = %v, want 42", em["value"])
	}
	if em["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", em["source"])
	}
}

func TestAddField_OverwritesExisting(t *testing.T) {
	m := make(map[string]any)
	addField(m, "k", "first", "yaml")
	addField(m, "k", "second", "override")

	em := m["k"].(map[string]any)
	if em["value"] != "second" {
		t.Errorf("value = %v, want second", em["value"])
	}
	if em["source"] != "override" {
		t.Errorf("source = %v, want override", em["source"])
	}
}

func TestApplyOverrides_UpdatesExistingKeys(t *testing.T) {
	m := make(map[string]any)
	addField(m, "collector.interval_seconds", 30, "yaml")
	addField(m, "trust.level", "observation", "yaml")

	overrides := []ConfigOverride{
		{Key: "collector.interval_seconds", Value: "60"},
		{Key: "trust.level", Value: "advisory"},
	}

	applyOverrides(m, overrides, "override")

	// collector.interval_seconds should be coerced to int.
	entry := m["collector.interval_seconds"].(map[string]any)
	if entry["value"] != 60 {
		t.Errorf("collector.interval_seconds = %v (%T), want 60 (int)",
			entry["value"], entry["value"])
	}
	if entry["source"] != "override" {
		t.Errorf("source = %v, want override", entry["source"])
	}

	// trust.level stays as string (type is "trust_level").
	trustEntry := m["trust.level"].(map[string]any)
	if trustEntry["value"] != "advisory" {
		t.Errorf("trust.level = %v, want advisory", trustEntry["value"])
	}
	if trustEntry["source"] != "override" {
		t.Errorf("trust.level source = %v, want override",
			trustEntry["source"])
	}
}

func TestApplyOverrides_IgnoresUnknownKeys(t *testing.T) {
	m := make(map[string]any)
	addField(m, "collector.interval_seconds", 30, "yaml")

	overrides := []ConfigOverride{
		{Key: "nonexistent.key", Value: "value"},
	}

	applyOverrides(m, overrides, "override")

	// The unknown key should not appear in the map.
	if _, ok := m["nonexistent.key"]; ok {
		t.Error("applyOverrides added unknown key to map")
	}

	// Existing key should be unchanged.
	entry := m["collector.interval_seconds"].(map[string]any)
	if entry["value"] != 30 {
		t.Errorf("existing key changed: got %v, want 30", entry["value"])
	}
	if entry["source"] != "yaml" {
		t.Errorf("source changed: got %v, want yaml", entry["source"])
	}
}

func TestApplyOverrides_EmptyOverrides(t *testing.T) {
	m := make(map[string]any)
	addField(m, "collector.interval_seconds", 30, "yaml")

	applyOverrides(m, nil, "override")
	applyOverrides(m, []ConfigOverride{}, "override")

	entry := m["collector.interval_seconds"].(map[string]any)
	if entry["value"] != 30 {
		t.Errorf("value changed after empty overrides: %v", entry["value"])
	}
	if entry["source"] != "yaml" {
		t.Errorf("source changed after empty overrides: %v", entry["source"])
	}
}

func TestApplyOverrides_CoercesBoolValues(t *testing.T) {
	m := make(map[string]any)
	addField(m, "llm.enabled", false, "yaml")
	addField(m, "trust.tier3_safe", true, "yaml")

	overrides := []ConfigOverride{
		{Key: "llm.enabled", Value: "true"},
		{Key: "trust.tier3_safe", Value: "false"},
	}

	applyOverrides(m, overrides, "override")

	llm := m["llm.enabled"].(map[string]any)
	if llm["value"] != true {
		t.Errorf("llm.enabled = %v (%T), want true (bool)",
			llm["value"], llm["value"])
	}

	trust := m["trust.tier3_safe"].(map[string]any)
	if trust["value"] != false {
		t.Errorf("trust.tier3_safe = %v (%T), want false (bool)",
			trust["value"], trust["value"])
	}
}

func TestApplyOverrides_CoercesFloatValues(t *testing.T) {
	m := make(map[string]any)
	addField(m, "analyzer.cache_hit_ratio_warning", 0.95, "yaml")

	overrides := []ConfigOverride{
		{Key: "analyzer.cache_hit_ratio_warning", Value: "0.85"},
	}

	applyOverrides(m, overrides, "db_override")

	entry := m["analyzer.cache_hit_ratio_warning"].(map[string]any)
	got, ok := entry["value"].(float64)
	if !ok {
		t.Fatalf("cache_hit_ratio_warning value is %T, want float64",
			entry["value"])
	}
	if got != 0.85 {
		t.Errorf("cache_hit_ratio_warning = %v, want 0.85", got)
	}
	if entry["source"] != "db_override" {
		t.Errorf("source = %v, want db_override", entry["source"])
	}
}

func TestApplyOverrides_SourceLabel(t *testing.T) {
	m := make(map[string]any)
	addField(m, "safety.cpu_ceiling_pct", 80, "yaml")

	overrides := []ConfigOverride{
		{Key: "safety.cpu_ceiling_pct", Value: "90"},
	}

	applyOverrides(m, overrides, "db_override")

	entry := m["safety.cpu_ceiling_pct"].(map[string]any)
	if entry["source"] != "db_override" {
		t.Errorf("source = %q, want %q", entry["source"], "db_override")
	}
}

func TestCoerceValue_IntTypes(t *testing.T) {
	tests := []struct {
		key   string
		value string
		want  int
	}{
		{"collector.interval_seconds", "30", 30},
		{"collector.batch_size", "100", 100},
		{"analyzer.slow_query_threshold_ms", "0", 0},
		{"safety.cpu_ceiling_pct", "80", 80},
	}

	for _, tt := range tests {
		got := coerceValue(tt.key, tt.value)
		n, ok := got.(int)
		if !ok {
			t.Errorf("coerceValue(%q, %q) = %T, want int",
				tt.key, tt.value, got)
			continue
		}
		if n != tt.want {
			t.Errorf("coerceValue(%q, %q) = %d, want %d",
				tt.key, tt.value, n, tt.want)
		}
	}
}

func TestCoerceValue_Float(t *testing.T) {
	got := coerceValue("analyzer.cache_hit_ratio_warning", "0.99")
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("coerceValue float01 returned %T, want float64", got)
	}
	if f != 0.99 {
		t.Errorf("coerceValue float01 = %v, want 0.99", f)
	}
}

func TestCoerceValue_Bool(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"True", true},
		{"TRUE", true},
		{"false", false},
		{"False", false},
	}

	for _, tt := range tests {
		got := coerceValue("llm.enabled", tt.value)
		b, ok := got.(bool)
		if !ok {
			t.Errorf("coerceValue(llm.enabled, %q) = %T, want bool",
				tt.value, got)
			continue
		}
		if b != tt.want {
			t.Errorf("coerceValue(llm.enabled, %q) = %v, want %v",
				tt.value, b, tt.want)
		}
	}
}

func TestCoerceValue_StringPassthrough(t *testing.T) {
	got := coerceValue("trust.maintenance_window", "02:00-06:00")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("coerceValue string type returned %T", got)
	}
	if s != "02:00-06:00" {
		t.Errorf("got %q, want %q", s, "02:00-06:00")
	}
}

func TestCoerceValue_InvalidIntFallsBackToString(t *testing.T) {
	got := coerceValue("collector.interval_seconds", "not-a-number")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("coerceValue with invalid int returned %T, want string",
			got)
	}
	if s != "not-a-number" {
		t.Errorf("got %q, want %q", s, "not-a-number")
	}
}

func TestCoerceValue_UnknownKeyReturnsString(t *testing.T) {
	got := coerceValue("unknown.key", "anyvalue")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("coerceValue unknown key returned %T, want string", got)
	}
	if s != "anyvalue" {
		t.Errorf("got %q, want %q", s, "anyvalue")
	}
}

func TestMaskSecret_Extended(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Short strings (<=4 chars) fully masked.
		{"", ""},
		{"a", "*"},
		{"ab", "**"},
		{"abc", "***"},
		{"abcd", "****"},
		// Longer strings: mask all but last 4.
		{"abcde", "*bcde"}, // wait, let me recalculate
		{"12345", "*2345"},
		{"sk-secret-key-12345678", "******************5678"},
	}

	for _, tt := range tests {
		got := maskSecret(tt.input)
		// For strings <= 4: all asterisks of same length.
		if len(tt.input) <= 4 {
			if len(got) != len(tt.input) {
				t.Errorf("maskSecret(%q) length = %d, want %d",
					tt.input, len(got), len(tt.input))
			}
			for _, c := range got {
				if c != '*' {
					t.Errorf("maskSecret(%q) = %q, expected all asterisks",
						tt.input, got)
					break
				}
			}
			continue
		}
		// For strings > 4: last 4 chars visible.
		wantSuffix := tt.input[len(tt.input)-4:]
		gotSuffix := got[len(got)-4:]
		if gotSuffix != wantSuffix {
			t.Errorf("maskSecret(%q) suffix = %q, want %q",
				tt.input, gotSuffix, wantSuffix)
		}
		// Prefix should all be asterisks.
		prefix := got[:len(got)-4]
		for _, c := range prefix {
			if c != '*' {
				t.Errorf("maskSecret(%q) prefix %q contains non-asterisk",
					tt.input, prefix)
				break
			}
		}
		// Total length preserved.
		if len(got) != len(tt.input) {
			t.Errorf("maskSecret(%q) length = %d, want %d",
				tt.input, len(got), len(tt.input))
		}
	}
}

func TestNullIfEmpty_Extended(t *testing.T) {
	got := nullIfEmpty("")
	if got != nil {
		t.Errorf("nullIfEmpty(\"\") = %v, want nil", got)
	}

	got = nullIfEmpty("value")
	if got == nil {
		t.Fatal("nullIfEmpty(\"value\") = nil, want non-nil")
	}
	if *got != "value" {
		t.Errorf("nullIfEmpty(\"value\") = %q, want %q", *got, "value")
	}
}

func TestValidateConfigKey_AllKeys(t *testing.T) {
	// Valid keys should not error.
	for key := range allowedConfigKeys {
		if err := validateConfigKey(key); err != nil {
			t.Errorf("validateConfigKey(%q) unexpected error: %v",
				key, err)
		}
	}

	// Invalid keys should error.
	invalid := []string{
		"", "unknown", "collector", "collector.",
		"collector.nonexistent", "foo.bar.baz",
	}
	for _, key := range invalid {
		if err := validateConfigKey(key); err == nil {
			t.Errorf("validateConfigKey(%q) = nil, want error", key)
		}
	}
}

func TestValidateConfigValue_Comprehensive(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		wantErr bool
	}{
		// int_pos
		{"collector.batch_size", "1", false},
		{"collector.batch_size", "100", false},
		{"collector.batch_size", "0", true},
		{"collector.batch_size", "-1", true},
		{"collector.batch_size", "abc", true},

		// int_min5
		{"collector.interval_seconds", "5", false},
		{"collector.interval_seconds", "60", false},
		{"collector.interval_seconds", "4", true},
		{"collector.interval_seconds", "0", true},

		// int_nonneg
		{"analyzer.slow_query_threshold_ms", "0", false},
		{"analyzer.slow_query_threshold_ms", "100", false},
		{"analyzer.slow_query_threshold_ms", "-1", true},

		// pct (0-100)
		{"analyzer.index_bloat_threshold_pct", "0", false},
		{"analyzer.index_bloat_threshold_pct", "50", false},
		{"analyzer.index_bloat_threshold_pct", "100", false},
		{"analyzer.index_bloat_threshold_pct", "101", true},
		{"analyzer.index_bloat_threshold_pct", "-1", true},

		// pct1_100 (1-100)
		{"safety.cpu_ceiling_pct", "1", false},
		{"safety.cpu_ceiling_pct", "100", false},
		{"safety.cpu_ceiling_pct", "0", true},

		// float01
		{"analyzer.cache_hit_ratio_warning", "0", false},
		{"analyzer.cache_hit_ratio_warning", "0.5", false},
		{"analyzer.cache_hit_ratio_warning", "1", false},
		{"analyzer.cache_hit_ratio_warning", "1.1", true},
		{"analyzer.cache_hit_ratio_warning", "-0.1", true},
		{"analyzer.cache_hit_ratio_warning", "abc", true},

		// bool
		{"llm.enabled", "true", false},
		{"llm.enabled", "false", false},
		{"llm.enabled", "True", false},
		{"llm.enabled", "FALSE", false},
		{"llm.enabled", "yes", true},
		{"llm.enabled", "1", true},

		// trust_level
		{"trust.level", "observation", false},
		{"trust.level", "advisory", false},
		{"trust.level", "autonomous", false},
		{"trust.level", "invalid", true},

		// exec_mode
		{"execution_mode", "auto", false},
		{"execution_mode", "approval", false},
		{"execution_mode", "manual", false},
		{"execution_mode", "invalid", true},

		// string (always valid)
		{"trust.maintenance_window", "", false},
		{"trust.maintenance_window", "anything", false},
		{"llm.endpoint", "https://example.com", false},

		// unknown key
		{"unknown.key", "value", true},
	}

	for _, tt := range tests {
		err := validateConfigValue(tt.key, tt.value)
		if tt.wantErr && err == nil {
			t.Errorf("validateConfigValue(%q, %q) = nil, want error",
				tt.key, tt.value)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("validateConfigValue(%q, %q) = %v, want nil",
				tt.key, tt.value, err)
		}
	}
}

// scanOverrideRows and scanAuditRows require pgx.Rows which cannot
// be easily mocked without a real database connection or a pgx mock
// library. These functions are covered by integration tests in
// config_store_integration_test.go.
