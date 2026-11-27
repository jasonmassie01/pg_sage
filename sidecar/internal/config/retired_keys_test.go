package config

import (
	"reflect"
	"strings"
	"testing"
)

// indexes-11: llm.optimizer.over_indexed_ratio_pct and plan_source, and
// llm.index_optimizer.over_indexed_ratio_pct, had no reader. They are gone
// from the struct; a config that still sets one loads, with exactly one
// WARNING per key, and the value has no effect.

func TestRetiredOptimizerKeysLoadWithOneWarningEach(t *testing.T) {
	baseline, _, err := loadWithWarnings(t, "llm:\n  optimizer:\n    min_query_calls: 7\n")
	if err != nil {
		t.Fatalf("Load baseline: %v", err)
	}
	for _, tc := range []struct{ name, body, warning string }{
		{"plan_source auto", "llm:\n  optimizer:\n    plan_source: auto\n" +
			"    min_query_calls: 7\n", "llm.optimizer.plan_source"},
		{"over_indexed_ratio_pct", "llm:\n  optimizer:\n    over_indexed_ratio_pct: 80\n" +
			"    min_query_calls: 7\n", "llm.optimizer.over_indexed_ratio_pct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, out, err := loadWithWarnings(t, tc.body)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := strings.Count(out, "WARNING:"); got != 1 {
				t.Fatalf("output %q has %d warnings, want exactly 1", out, got)
			}
			if !strings.Contains(out, tc.warning) || !strings.Contains(out, "remove it") {
				t.Fatalf("warning %q does not name %s and tell the operator what to do",
					out, tc.warning)
			}
			if !reflect.DeepEqual(cfg.LLM.Optimizer, baseline.LLM.Optimizer) {
				t.Fatalf("retired key changed the optimizer config:\n%+v\nwant\n%+v",
					cfg.LLM.Optimizer, baseline.LLM.Optimizer)
			}
		})
	}
}

func TestRetiredLegacyIndexOptimizerKeyLoadsWithOneWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "llm:\n  index_optimizer:\n    enabled: true\n"+
		"    over_indexed_ratio_pct: 120\n    max_include_columns: 2\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, "WARNING:"); got != 1 ||
		!strings.Contains(out, "llm.index_optimizer.over_indexed_ratio_pct") {
		t.Fatalf("output %q, want one warning naming the legacy key", out)
	}
	if !cfg.LLM.Optimizer.Enabled || cfg.LLM.Optimizer.MaxIncludeColumns != 2 {
		t.Fatalf("legacy block must still migrate: enabled=%v max_include_columns=%d",
			cfg.LLM.Optimizer.Enabled, cfg.LLM.Optimizer.MaxIncludeColumns)
	}
}

// Every retired key in one file: one warning each, nothing else lost.
func TestRetiredKeysAcrossSectionsWarnOncePerKey(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "tuner:\n  analyze_maintenance_threshold_mb: 5\n"+
		"  analyze_cooldown_minutes: 45\nllm:\n  optimizer:\n    plan_source: none\n"+
		"    over_indexed_ratio_pct: 80\n    max_include_columns: 4\n"+
		"  index_optimizer:\n    over_indexed_ratio_pct: 90\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, "WARNING:"); got != 4 {
		t.Fatalf("output %q has %d warnings, want 4", out, got)
	}
	for _, key := range []string{"tuner.analyze_maintenance_threshold_mb",
		"llm.optimizer.plan_source", "llm.optimizer.over_indexed_ratio_pct",
		"llm.index_optimizer.over_indexed_ratio_pct"} {
		if strings.Count(out, key+" ") != 1 {
			t.Errorf("output %q must name %s exactly once", out, key)
		}
	}
	if cfg.Tuner.AnalyzeCooldownMinutes != 45 || cfg.LLM.Optimizer.MaxIncludeColumns != 4 {
		t.Fatalf("sibling keys lost: cooldown=%d max_include_columns=%d",
			cfg.Tuner.AnalyzeCooldownMinutes, cfg.LLM.Optimizer.MaxIncludeColumns)
	}
}

func TestMaxIncludeColumnsDefaultsToThreeWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Optimizer.MaxIncludeColumns != 3 {
		t.Fatalf("max_include_columns = %d without a config file, want 3",
			cfg.LLM.Optimizer.MaxIncludeColumns)
	}
}

func TestMaxIncludeColumnsZeroLoadsAndIsKept(t *testing.T) {
	// 0 is not a config error: the validator reads <= 0 as the default 3.
	cfg, out, err := loadWithWarnings(t, "llm:\n  optimizer:\n    max_include_columns: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Optimizer.MaxIncludeColumns != 0 || strings.Contains(out, "WARNING") {
		t.Fatalf("max_include_columns 0 = %d warnings %q", cfg.LLM.Optimizer.MaxIncludeColumns,
			out)
	}
}

func TestStripRetiredKeysInputs(t *testing.T) {
	if _, _, err := stripRetiredKeys("llm: [unclosed"); err == nil {
		t.Fatal("invalid YAML must be an error, not silently no warnings")
	}
	for name, raw := range map[string]string{
		"empty":                 "",
		"no retired key":        "llm:\n  optimizer:\n    min_query_calls: 5\n",
		"llm not a map":         "llm: ~\n",
		"optimizer not a map":   "llm:\n  optimizer: ~\n",
		"optimizer is a list":   "llm:\n  optimizer: [plan_source]\n",
		"key at the wrong path": "plan_source: auto\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings, err := stripRetiredKeys(raw)
			if err != nil || len(warnings) != 0 || got != raw {
				t.Fatalf("stripRetiredKeys(%q) = %q, %v, %v; want unchanged", raw, got,
					warnings, err)
			}
		})
	}
}

func TestStripRetiredKeysRemovesOnlyRetiredKeys(t *testing.T) {
	raw := "llm:\n  optimizer:\n    plan_source: auto\n    min_query_calls: 9\n"
	got, warnings, err := stripRetiredKeys(raw)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("stripRetiredKeys = %v, %v; want one warning", warnings, err)
	}
	if strings.Contains(got, "plan_source") || !strings.Contains(got, "min_query_calls: 9") {
		t.Fatalf("stripped YAML %q must drop plan_source and keep min_query_calls", got)
	}
}

// Stripping re-encodes only the first document, so a second document must
// still be refused rather than silently dropped with the retired key.
func TestRetiredKeyDoesNotHideASecondDocument(t *testing.T) {
	for name, body := range map[string]string{
		"optimizer key": "llm:\n  optimizer:\n    plan_source: auto\n---\nmode: x\n",
		"tuner key":     "tuner:\n  analyze_maintenance_threshold_mb: 5\n---\nmode: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadWithWarnings(t, body)
			if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
				t.Fatalf("Load = %v, want the one-document error", err)
			}
		})
	}
	if err := requireOneDocument("mode: x\n---\nllm: [unclosed\n"); err == nil ||
		!strings.Contains(err.Error(), "read config") {
		t.Fatalf("broken second document = %v, want a read error", err)
	}
	for _, one := range []string{"", "---\nmode: x\n", "mode: x\n...\n", "# only a comment\n"} {
		if err := requireOneDocument(one); err != nil {
			t.Fatalf("requireOneDocument(%q) = %v, want nil", one, err)
		}
	}
}

// No concurrency or state-transition tests: stripping is a pure function of
// the YAML text, run once per load.

// forecast-06: forecaster.min_data_points and alert_horizons fed only the
// retired v0.9 growth forecaster. A config that sets them still loads,
// with one "no effect, remove it" warning per key, and the live
// forecaster keys beside them keep their values.

func TestRetiredForecasterGrowthKeyLoadsWithOneWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t,
		"forecaster:\n  alert_horizons: [30]\n  disk_capacity_bytes: 1000\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, "WARNING:"); got != 1 {
		t.Fatalf("warnings %q: %d, want exactly one", out, got)
	}
	if !strings.Contains(out, "forecaster.alert_horizons") ||
		!strings.Contains(out, "no effect") || !strings.Contains(out, "remove it") {
		t.Fatalf("warning %q does not name the key and say it has no effect", out)
	}
	if cfg.Forecaster.DiskCapacityBytes != 1000 {
		t.Fatalf("disk_capacity_bytes = %d, want 1000: the rest of the block must load",
			cfg.Forecaster.DiskCapacityBytes)
	}
}

func TestRetiredForecasterGrowthKeysEachWarnOnce(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "forecaster:\n  min_data_points: 0\n"+
		"  alert_horizons: []\n  min_r_squared: 0.7\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Count(out, "forecaster.min_data_points") != 1 ||
		strings.Count(out, "forecaster.alert_horizons") != 1 ||
		strings.Count(out, "WARNING:") != 2 {
		t.Fatalf("warnings %q, want one per retired key", out)
	}
	if cfg.Forecaster.MinRSquared != 0.7 {
		t.Fatalf("min_r_squared = %v, want 0.7: it is not retired", cfg.Forecaster.MinRSquared)
	}
}

func TestLiveForecasterKeysGiveNoGrowthWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t,
		"forecaster:\n  min_r_squared: 0.6\n  disk_capacity_bytes: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(out, "WARNING:") {
		t.Fatalf("warnings %q for live forecaster keys", out)
	}
	if cfg.Forecaster.MinRSquared != 0.6 || cfg.Forecaster.DiskCapacityBytes != 0 {
		t.Fatalf("forecaster = %+v, want the values as written", cfg.Forecaster)
	}
}
