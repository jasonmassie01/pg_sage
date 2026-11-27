package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexes-05: llm.optimizer.plan_source no longer has a reader. A config
// that sets it still loads, with one warning per load telling the
// operator to remove it.

const planSourceWarning = "llm.optimizer.plan_source"

func loadWithWarnings(t *testing.T, body string) (*Config, string, error) {
	t.Helper()
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })
	cfg, err := Load([]string{"--config", path,
		"--pg-url", "postgres://u:p@db.example:5432/app"})
	return cfg, out.String(), err
}

func TestPlanSourceLoadsWithOneDeprecationWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t,
		"llm:\n  optimizer:\n    plan_source: generic_plan\n    min_query_calls: 7\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Count(out, planSourceWarning); got != 1 {
		t.Fatalf("warnings %q mention %s %d times, want once", out, planSourceWarning, got)
	}
	if !strings.Contains(out, "WARNING:") || !strings.Contains(out, "ignored") {
		t.Fatalf("warning %q does not say the key is ignored", out)
	}
	if cfg.LLM.Optimizer.MinQueryCalls != 7 {
		t.Fatalf("min_query_calls = %d, want 7: the rest of the block must still load",
			cfg.LLM.Optimizer.MinQueryCalls)
	}
}

func TestPlanSourceEmptyValueStillWarns(t *testing.T) {
	_, out, err := loadWithWarnings(t, "llm:\n  optimizer:\n    plan_source: \"\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Count(out, planSourceWarning) != 1 {
		t.Fatalf("warnings %q, want one about the set key", out)
	}
}

func TestPlanSourceAbsentGivesNoWarning(t *testing.T) {
	for name, body := range map[string]string{
		"no llm block":          "mode: standalone\n",
		"llm without optimizer": "llm:\n  enabled: false\n",
		"optimizer without key": "llm:\n  optimizer:\n    min_query_calls: 5\n",
		"key elsewhere":         "llm:\n  plan_source_note: x\n",
		"optimizer not a map":   "llm:\n  optimizer: ~\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, out, err := loadWithWarnings(t, body)
			if name == "key elsewhere" {
				if err == nil {
					t.Fatal("an unknown llm key must still be refused by the strict decode")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if strings.Contains(out, planSourceWarning) {
				t.Fatalf("warnings %q mention plan_source", out)
			}
		})
	}
}

// No concurrency or state-transition tests: the check is a pure function
// of the YAML text.
