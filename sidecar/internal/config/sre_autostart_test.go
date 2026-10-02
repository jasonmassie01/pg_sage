package config

import (
	"strings"
	"testing"
)

// Sage SRE M4 (R1 GA): automatic start is on by default. Investigations
// are read-only and bounded (catalog probes, 12-probe and 120 s
// ceilings), so starting one for each incident is the AI-DBA default.
// An explicit false is honoured, and nothing else in the sre section
// can turn it off by omission (default-value masking guard).

func TestSREAutoStart_DefaultConfigIsOn(t *testing.T) {
	if !DefaultConfig().SRE.AutomaticStart {
		t.Fatal("DefaultConfig().SRE.AutomaticStart = false, want true")
	}
}

func TestSREAutoStart_ExplicitFalseHonoured(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  automatic_start: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.AutomaticStart {
		t.Fatal("automatic_start: false was overridden by the default")
	}
	if !cfg.SRE.LLM.Enabled || cfg.SRE.TriggerIntervalSeconds != 15 {
		t.Fatalf("turning auto-start off changed other sre defaults: %+v", cfg.SRE)
	}
}

func TestSREAutoStart_ExplicitTrue(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  automatic_start: true\n")
	if err != nil || !cfg.SRE.AutomaticStart {
		t.Fatalf("automatic_start: true = %+v (%v)", cfg, err)
	}
}

func TestSREAutoStart_PartialSectionKeepsItOn(t *testing.T) {
	for _, body := range []string{
		"sre:\n  trigger_interval_seconds: 30\n",
		"sre:\n  llm:\n    enabled: false\n",
		"sre: {}\n",
	} {
		cfg, err := loadRCAYAML(t, body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if !cfg.SRE.AutomaticStart {
			t.Errorf("%q turned automatic start off by omission", body)
		}
	}
}

func TestSREAutoStart_InvalidValuesRejected(t *testing.T) {
	for _, bad := range []string{"sre:\n  automatic_start: maybe\n",
		"sre:\n  automatic_start: [true]\n"} {
		if _, err := loadRCAYAML(t, bad); err == nil {
			t.Errorf("accepted invalid sre.automatic_start %q", bad)
		}
	}
}

func TestSREAutoStart_DocTagSaysOnByDefault(t *testing.T) {
	doc := collectDocTags(t, DefaultConfig())["sre.automatic_start"]
	for _, want := range []string{"Default: true", "read-only", "false"} {
		if !strings.Contains(doc, want) {
			t.Errorf("sre.automatic_start doc %q lacks %q", doc, want)
		}
	}
	if strings.Contains(doc, "Default: false") {
		t.Errorf("sre.automatic_start doc still says Default: false: %q", doc)
	}
}
