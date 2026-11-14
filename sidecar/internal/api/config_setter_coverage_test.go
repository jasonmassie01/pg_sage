package api

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-10: deleting the stale setter groups must leave no allowed key
// without a setter. Every key the store accepts is applied with two valid
// values; the two results must differ, so the value reached a field.

var setterProbeValues = map[string][]string{
	"int_pos":            {"7", "11", "1", "2"},
	"int_nonneg":         {"0", "9", "1", "2"},
	"int_min5":           {"5", "17", "6", "60"},
	"int_week_minutes":   {"5", "90", "1", "10080"},      // rules-09
	"int_class_timeout":  {"60", "14400", "61", "86400"}, // executor-05
	"int_vacuum_stall":   {"5", "15", "6", "240"},        // vacuum-05
	"pct":                {"0", "37", "1", "99"},
	"pct1_100":           {"1", "63", "2", "99"},
	"float01":            {"0.25", "0.75", "0.5"},
	"float_share":        {"0.5", "0.75", "1"}, // vacuum-08
	"float_pos":          {"1.5", "2.5", "0.5"},
	"float_nonneg":       {"0", "3.5", "1.25"},
	"float_pct_pos":      {"0.5", "42.5", "10"},
	"float_pct_open":     {"0.5", "42.5", "99.5"}, // vacuum-11b
	"bool":               {"true", "false"},
	"policy_profile":     {"staffed", "unattended"},
	"clone_provider":     {"none", "dle", "snapshot"},
	"mcp_transport":      {"stdio", "http"},
	"llm_auth":           {"api_key", "databricks"},
	"maintenance_window": {"never", "Sun 02:00-06:00"},
	"rule_list":          {"unused_indexes", "slow_queries,temp_spills"}, // market-11
	"string":             {"probe-a", "probe-b"},
}

// setterProbePair returns two values the store accepts for key.
func setterProbePair(t *testing.T, key, vtype string) (string, string) {
	t.Helper()
	candidates := setterProbeValues[vtype]
	if vtype == "trust_level" {
		candidates = config.TrustLevels()
	}
	var valid []string
	for _, value := range candidates {
		if store.ValidateConfigOverride(key, value) == nil {
			valid = append(valid, value)
		}
	}
	if len(valid) < 2 {
		t.Fatalf("%s (%s): need two valid probe values, have %v of %v",
			key, vtype, valid, candidates)
	}
	return valid[0], valid[1]
}

func TestEveryAllowedKeyHasASetter(t *testing.T) {
	allowed := store.AllowedConfigKeysSnapshot()
	if len(allowed) < 100 {
		t.Fatalf("allowed key registry has %d keys; expected the full set", len(allowed))
	}
	keys := make([]string, 0, len(allowed))
	for key := range allowed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			first, second := setterProbePair(t, key, allowed[key])
			a := config.DefaultConfig()
			b := config.DefaultConfig()
			ApplyConfigOverrideSnapshot(a, key, first)
			ApplyConfigOverrideSnapshot(b, key, second)
			if reflect.DeepEqual(a, b) {
				t.Fatalf("%s: applying %q and %q produced the same config; "+
					"the key has no setter", key, first, second)
			}
		})
	}
}

func TestSetterRoundTripsThroughReadModel(t *testing.T) {
	cases := map[string]string{
		"collector.interval_seconds":          "45",
		"analyzer.cache_hit_ratio_warning":    "0.75",
		"trust.level":                         "advisory",
		"safety.cpu_ceiling_pct":              "63",
		"llm.optimizer.max_new_per_table":     "4",
		"retention.explains_days":             "21",
		"rca.wal_spike_multiplier":            "2.5",
		"migration.mode":                      "probe-mode",
		"custodian.wal.abandon_after_minutes": "33",
		"mcp.transport":                       "http",
	}
	for key, value := range cases {
		cfg := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(cfg, key, value)
		field, ok := store.ConfigReadModel(cfg)[key].(map[string]any)
		if !ok {
			t.Errorf("%s missing from the read model", key)
			continue
		}
		if got := fieldString(field["value"]); got != value {
			t.Errorf("%s read back %q after applying %q", key, got, value)
		}
	}
}

// Keys the store rejects never reach a field, even when a stale row
// carries one (config-02 skips them; config-10 deleted their setters).
func TestRetiredKeysHaveNoSetter(t *testing.T) {
	retired := map[string]string{
		"llm.optimizer_llm.enabled":             "true",
		"llm.optimizer_llm.endpoint":            "https://stale.example/v1",
		"llm.optimizer_llm.model":               "stale-model",
		"llm.optimizer_llm.token_budget_daily":  "7",
		"llm.optimizer_llm.max_output_tokens":   "9",
		"llm.optimizer_llm.fallback_to_general": "true",
		"briefing.schedule":                     "0 1 * * *",
		"briefing.slack_webhook_url":            "https://hooks.stale.example/x",
		"forecaster.enabled":                    "false",
		"forecaster.lookback_days":              "3",
		"forecaster.disk_warn_growth_gb_day":    "0.125",
		"auto_explain.enabled":                  "true",
		"auto_explain.log_min_duration_ms":      "3",
		"auto_explain.collect_interval_seconds": "3",
		"auto_explain.max_plans_per_cycle":      "3",
		"tuner.enabled":                         "false",
		"tuner.llm_enabled":                     "false",
		"tuner.work_mem_max_mb":                 "3",
		"tuner.plan_time_ratio":                 "0.125",
		"tuner.nested_loop_row_threshold":       "3",
		"tuner.parallel_min_table_rows":         "3",
		"tuner.min_query_calls":                 "3",
		"tuner.verify_after_apply":              "false",
		"alerting.timezone":                     "Pacific/Chatham",
	}
	allowed := store.AllowedConfigKeysSnapshot()
	for key, value := range retired {
		if _, ok := allowed[key]; ok {
			t.Errorf("%s is an allowed key; it needs a setter, not this test", key)
			continue
		}
		cfg := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(cfg, key, value)
		if !reflect.DeepEqual(cfg, config.DefaultConfig()) {
			t.Errorf("%s=%q changed the config; a stale setter is still wired", key, value)
		}
	}
}

func TestSetterIgnoresUnknownAndEmptyKeys(t *testing.T) {
	for _, key := range []string{"", ".", "collector.", "collector.unknown", "nope"} {
		cfg := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(cfg, key, "42")
		if !reflect.DeepEqual(cfg, config.DefaultConfig()) {
			t.Errorf("key %q changed the config", key)
		}
	}
}

func TestAPITrustSetterAcceptsExactlyConfigTrustLevels(t *testing.T) {
	for _, level := range config.TrustLevels() {
		cfg := config.DefaultConfig()
		cfg.Trust.Level = "unset"
		ApplyConfigOverrideSnapshot(cfg, "trust.level", level)
		if cfg.Trust.Level != level {
			t.Errorf("trust.level %q not applied: got %q", level, cfg.Trust.Level)
		}
	}
	for _, level := range []string{"", "Autonomous", "auto", "manual", "observe"} {
		cfg := config.DefaultConfig()
		ApplyConfigOverrideSnapshot(cfg, "trust.level", level)
		if cfg.Trust.Level != config.DefaultTrustLevel {
			t.Errorf("trust.level %q applied: got %q", level, cfg.Trust.Level)
		}
	}
}

func fieldString(value any) string { return fmt.Sprint(value) }
