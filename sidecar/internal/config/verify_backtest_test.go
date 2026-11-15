package config

import (
	"strings"
	"testing"
)

// lab-02: verify.backtest_max_runtime_minutes (10) bounds one verifier
// back-test run and verify.backtest_placebos (300) is how many placebos
// the weekly run draws. Plain integers read once at load: no shared
// state, so no concurrent-access test applies.

func TestVerifyBacktest_DefaultsWithoutAConfigFile(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Verify.BacktestMaxRuntimeMinutes != 10 || cfg.Verify.BacktestPlacebos != 300 {
		t.Fatalf("defaults: %d minutes, %d placebos; want 10 and 300",
			cfg.Verify.BacktestMaxRuntimeMinutes, cfg.Verify.BacktestPlacebos)
	}
	chdirTemp(t)
	loaded, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Verify.BacktestMaxRuntimeMinutes != 10 || loaded.Verify.BacktestPlacebos != 300 {
		t.Fatalf("loaded with no file: %d minutes, %d placebos",
			loaded.Verify.BacktestMaxRuntimeMinutes, loaded.Verify.BacktestPlacebos)
	}
	partial, err := loadRCAYAML(t, "verify:\n  min_samples: 5\n")
	if err != nil || partial.Verify.BacktestPlacebos != 300 ||
		partial.Verify.BacktestMaxRuntimeMinutes != 10 {
		t.Fatalf("a partial verify section masked the defaults: %+v %v", partial.Verify, err)
	}
}

func TestVerifyBacktest_Bounds(t *testing.T) {
	cases := []struct {
		yaml string
		ok   bool
		want string
	}{
		{"backtest_max_runtime_minutes: 1", true, ""},
		{"backtest_max_runtime_minutes: 1440", true, ""},
		{"backtest_max_runtime_minutes: 0", false, "verify.backtest_max_runtime_minutes"},
		{"backtest_max_runtime_minutes: 1441", false, "verify.backtest_max_runtime_minutes"},
		{"backtest_placebos: 200", true, ""},
		{"backtest_placebos: 10000", true, ""},
		{"backtest_placebos: 199", false, "verify.backtest_placebos"},
		{"backtest_placebos: 10001", false, "verify.backtest_placebos"},
		{"backtest_placebos: -5", false, "verify.backtest_placebos"},
	}
	for _, tc := range cases {
		_, err := loadRCAYAML(t, "verify:\n  "+tc.yaml+"\n")
		if tc.ok && err != nil {
			t.Errorf("%s refused: %v", tc.yaml, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want an error naming %s", tc.yaml, err, tc.want)
		}
	}
}
