package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tuning-09: tuner.revalidation_keep_ratio, revalidation_rollback_ratio and
// revalidation_explain_timeout_ms never had an effect (revalidation does no
// hinted-vs-unhinted cost comparison and issues no EXPLAIN). They are
// retired: a config that still sets one loads, the key is dropped, and one
// actionable warning per key is written.

var tuning09RetiredKeys = []string{
	"revalidation_keep_ratio",
	"revalidation_rollback_ratio",
	"revalidation_explain_timeout_ms",
}

// loadWithWarnings is shared with ignored_keys_test.go.

func retiredWarning(key string) string {
	return "tuner." + key + " is no longer used and is ignored; remove it"
}

func TestRetiredKeepRatioKeyLoadsWithOneWarning(t *testing.T) {
	cfg, warnings, err := loadWithWarnings(t,
		"tuner:\n  revalidation_keep_ratio: 1.2\n  hint_retirement_days: 9\n")
	if err != nil {
		t.Fatalf("Load with retired key: %v", err)
	}
	if cfg.Tuner.HintRetirementDays != 9 {
		t.Fatalf("sibling key lost: hint_retirement_days = %d, want 9",
			cfg.Tuner.HintRetirementDays)
	}
	if got := strings.Count(warnings, "WARNING:"); got != 1 {
		t.Fatalf("WARNING count = %d in %q, want exactly 1", got, warnings)
	}
	if !strings.Contains(warnings, retiredWarning("revalidation_keep_ratio")) {
		t.Fatalf("warning %q does not name tuner.revalidation_keep_ratio", warnings)
	}
}

// All three keys together: three warnings, one per key, and the config
// still loads with its other tuner values.
func TestRetiredRevalidationKeysEachWarnOnce(t *testing.T) {
	yaml := "tuner:\n  revalidation_keep_ratio: 1.2\n" +
		"  revalidation_rollback_ratio: 0.8\n" +
		"  revalidation_explain_timeout_ms: 10000\n" +
		"  nested_loop_row_threshold: 25000\n"
	cfg, warnings, err := loadWithWarnings(t, yaml)
	if err != nil {
		t.Fatalf("Load with retired keys: %v", err)
	}
	if cfg.Tuner.NestedLoopRowThreshold != 25000 {
		t.Fatalf("nested_loop_row_threshold = %d, want 25000",
			cfg.Tuner.NestedLoopRowThreshold)
	}
	for _, key := range tuning09RetiredKeys {
		if got := strings.Count(warnings, retiredWarning(key)); got != 1 {
			t.Errorf("warning for %s appears %d times in %q, want 1", key, got, warnings)
		}
	}
}

// Invalid values for a retired key are not validated: the key is dropped
// before the strict decode, so a wrong type cannot fail the load.
func TestRetiredRevalidationKeyWithBadValueStillLoads(t *testing.T) {
	_, warnings, err := loadWithWarnings(t,
		"tuner:\n  revalidation_explain_timeout_ms: \"ten seconds\"\n")
	if err != nil {
		t.Fatalf("Load with a mistyped retired key: %v", err)
	}
	if !strings.Contains(warnings, retiredWarning("revalidation_explain_timeout_ms")) {
		t.Fatalf("missing warning in %q", warnings)
	}
}

// Defaults without any config file: nested_loop_row_threshold keeps its
// 10,000 default and no warning is written.
func TestNestedLoopThresholdDefaultWithoutConfig(t *testing.T) {
	cfg, warnings, err := loadWithWarnings(t, "tuner:\n  enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tuner.NestedLoopRowThreshold != 10000 {
		t.Fatalf("nested_loop_row_threshold default = %d, want 10000",
			cfg.Tuner.NestedLoopRowThreshold)
	}
	for _, key := range tuning09RetiredKeys {
		if strings.Contains(warnings, key) {
			t.Fatalf("unexpected warning for %s: %q", key, warnings)
		}
	}
}

// Shipped example configs and user docs no longer carry the keys.
func TestShippedFilesOmitRetiredRevalidationKeys(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, rel := range []string{
		filepath.Join("sidecar", "config.example.yaml"),
		filepath.Join("docs", "generated", "config-lifecycles.md"),
		filepath.Join("sidecar", "web", "src", "generated", "config_meta.json"),
	} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, key := range tuning09RetiredKeys {
			if strings.Contains(string(body), key) {
				t.Errorf("%s still names tuner.%s", rel, key)
			}
		}
	}
}
