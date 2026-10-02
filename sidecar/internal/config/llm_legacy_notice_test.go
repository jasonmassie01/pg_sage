package config

import (
	"strings"
	"testing"
)

// The deprecated llm.index_optimizer block migrates to llm.optimizer only
// for the keys a file actually sets, so an explicit value is never
// overridden by a default.

func TestLegacyIndexOptimizerFalseDisablesOptimizer(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  index_optimizer:\n    enabled: false\n")
	if cfg.LLM.Optimizer.Enabled {
		t.Fatal("explicit llm.index_optimizer.enabled=false left the optimizer on")
	}
}

func TestLegacyIndexOptimizerTrueCopiesTuning(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  index_optimizer:\n    enabled: true\n"+
		"    min_query_calls: 42\n    max_indexes_per_table: 7\n"+
		"    max_include_columns: 2\n    over_indexed_ratio_pct: 120\n"+
		"    write_heavy_ratio_pct: 60\n")
	opt := cfg.LLM.Optimizer
	if !opt.Enabled {
		t.Fatal("legacy enabled=true did not enable the optimizer")
	}
	got := []int{opt.MinQueryCalls, opt.MaxIndexesPerTable,
		opt.MaxIncludeColumns, opt.OverIndexedRatioPct, opt.WriteHeavyRatioPct}
	want := []int{42, 7, 2, 120, 60}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("migrated tuning = %v, want %v", got, want)
			break
		}
	}
}

// The current key wins over the deprecated one, in both directions.
func TestOptimizerKeyWinsOverLegacyIndexOptimizer(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"llm:\n  optimizer:\n    enabled: false\n" +
			"  index_optimizer:\n    enabled: true\n", false},
		{"llm:\n  optimizer:\n    enabled: true\n" +
			"  index_optimizer:\n    enabled: false\n", true},
	} {
		cfg := loadYAMLConfig(t, tc.body)
		if cfg.LLM.Optimizer.Enabled != tc.want {
			t.Errorf("%q: optimizer.enabled = %v, want %v", tc.body,
				cfg.LLM.Optimizer.Enabled, tc.want)
		}
	}
}

// Legacy tuning without the legacy enabled key changes nothing.
func TestLegacyTuningWithoutEnabledKeyIsIgnored(t *testing.T) {
	cfg := loadYAMLConfig(t, "llm:\n  index_optimizer:\n    min_query_calls: 5\n")
	if !cfg.LLM.Optimizer.Enabled {
		t.Error("optimizer default turned off by a legacy tuning key")
	}
	if cfg.LLM.Optimizer.MinQueryCalls != DefaultOptMinQueryCalls {
		t.Errorf("min_query_calls = %d, want default %d",
			cfg.LLM.Optimizer.MinQueryCalls, DefaultOptMinQueryCalls)
	}
}

func TestLLMSetupNoticeWhenEnabledButUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, key string
	}{
		{"neither", "", ""},
		{"endpoint only", "https://llm.example/v1", ""},
		{"key only", "", "k"},
	} {
		cfg := DefaultConfig()
		cfg.LLM.Endpoint, cfg.LLM.APIKey = tc.endpoint, tc.key
		note := cfg.LLMSetupNotice()
		for _, want := range []string{
			"llm.endpoint", "llm.api_key", "SAGE_LLM_API_KEY",
			"deterministic", "llm.enabled: false",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: notice %q lacks %q", tc.name, note, want)
			}
		}
		if strings.Contains(note, "\n") {
			t.Errorf("%s: notice spans lines: %q", tc.name, note)
		}
	}
}

func TestLLMSetupNoticeSilentWhenConfiguredOrDisabled(t *testing.T) {
	configured := DefaultConfig()
	configured.LLM.Endpoint = "https://llm.example/v1"
	configured.LLM.APIKey = "k"
	if note := configured.LLMSetupNotice(); note != "" {
		t.Errorf("configured LLM notice = %q, want none", note)
	}
	disabled := DefaultConfig()
	disabled.LLM.Enabled = false
	if note := disabled.LLMSetupNotice(); note != "" {
		t.Errorf("disabled LLM notice = %q, want none", note)
	}
	var nilCfg *Config
	if note := nilCfg.LLMSetupNotice(); note != "" {
		t.Errorf("nil config notice = %q, want none", note)
	}
}
