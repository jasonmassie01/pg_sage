package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// The generated config metadata carries each key's self-configuration
// class, and the generator writes the derived-settings reference.

func TestGenerateCarriesTheSelfConfigClass(t *testing.T) {
	meta, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"collector.interval_seconds":   "derivable",
		"trust.level":                  "safety_critical",
		"llm.api_key":                  "safety_critical",
		"alerting.quiet_hours_start":   "operator_preference",
		"self_config.soak_hours":       "safety_critical",
		"sre.detectors.lwlock_waiters": "derivable",
		"databases[].trust_level":      "safety_critical",
	} {
		if got := meta[key].Class; got != want {
			t.Errorf("%s class %q, want %q", key, got, want)
		}
	}
	for key, m := range meta {
		if m.Class == "" {
			t.Errorf("%s has no class in the generated metadata", key)
		}
	}
}

func TestRunWritesTheDerivedSettingsReference(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/derived-settings.md"
	if err := run([]string{"-out", dir + "/config_meta.json", "-derived-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != selfconfig.Markdown() {
		t.Fatal("derived-settings output does not match selfconfig.Markdown()")
	}
	if err := run([]string{"-out", dir + "/m.json", "-derived-out",
		dir + "/missing/dir/\x00/x.md"}); err == nil {
		t.Fatal("an unwritable derived-settings path was accepted")
	}
	if !strings.Contains(string(body), "Derived settings") {
		t.Fatal("reference has no title")
	}
}
