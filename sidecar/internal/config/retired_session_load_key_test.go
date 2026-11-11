package config

import (
	"strings"
	"testing"
)

// rca-04: auto_explain.prefer_session_load never had a reader (the
// on-demand EXPLAIN is not logged by auto_explain, so loading the module
// for it did nothing). A config that still sets it loads, whatever its
// value, with one warning; the rest of the auto_explain block still loads.

const preferSessionLoadWarning = "auto_explain.prefer_session_load"

func TestRetiredSessionLoadKeyLoadsWithOneWarning(t *testing.T) {
	for _, value := range []string{"true", "false", "\"yes please\"", "7"} {
		cfg, out, err := loadWithWarnings(t, "auto_explain:\n  prefer_session_load: "+
			value+"\n  max_plans_per_cycle: 4\n")
		if err != nil {
			t.Fatalf("prefer_session_load %s: Load: %v", value, err)
		}
		if got := strings.Count(out, preferSessionLoadWarning); got != 1 {
			t.Fatalf("prefer_session_load %s: warnings %q mention it %d times, want once",
				value, out, got)
		}
		if !strings.Contains(out, "WARNING:") || !strings.Contains(out, "ignored") {
			t.Fatalf("warning %q does not say the key is ignored", out)
		}
		if cfg.AutoExplain.MaxPlansPerCycle != 4 {
			t.Fatalf("max_plans_per_cycle = %d, want 4: the rest of the block must load",
				cfg.AutoExplain.MaxPlansPerCycle)
		}
	}
}

// Defaults: with no auto_explain block the defaults hold and no warning
// mentions the retired key.
func TestAutoExplainWithoutRetiredSessionLoadKeyHasNoWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "mode: standalone\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(out, preferSessionLoadWarning) {
		t.Fatalf("warnings %q mention prefer_session_load, which is not set", out)
	}
	if !cfg.AutoExplain.Enabled ||
		cfg.AutoExplain.MaxPlansPerCycle != DefaultAutoExplainMaxPlansPerCycle {
		t.Fatalf("auto_explain defaults lost: %+v", cfg.AutoExplain)
	}
}

// The same key elsewhere is not the retired key: the strict decode rejects it.
func TestRetiredSessionLoadKeyElsewhereIsStillUnknown(t *testing.T) {
	_, out, err := loadWithWarnings(t, "analyzer:\n  prefer_session_load: true\n")
	if err == nil {
		t.Fatal("analyzer.prefer_session_load was accepted")
	}
	if strings.Contains(out, preferSessionLoadWarning) {
		t.Fatalf("warnings %q treat a different key as the retired one", out)
	}
}
