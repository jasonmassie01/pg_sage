package config

import (
	"strings"
	"testing"
)

// sre.investigator.require_support_citation (sre-03 Phase 2): on by
// default, restart-scoped. A model contest or conclusion keeps its root
// only when a kept claim cites evidence the graph records as support for
// it. An explicit false is honoured; a bad value or an unknown key in the
// block names the key.

// No concurrent-access tests: config loading is single-threaded and the
// value is read once at startup.

func TestSRESupportCitation_DefaultIsOn(t *testing.T) {
	if !DefaultConfig().SRE.Investigator.RequireSupportCitation {
		t.Fatal("DefaultConfig().SRE.Investigator.RequireSupportCitation = false, want true")
	}
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load without a config file: %v", err)
	}
	if !cfg.SRE.Investigator.RequireSupportCitation {
		t.Fatal("no config file: require_support_citation defaulted to false")
	}
}

func TestSRESupportCitation_ExplicitValuesHonoured(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  investigator:\n    require_support_citation: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.Investigator.RequireSupportCitation {
		t.Fatal("require_support_citation: false was overridden by the default")
	}
	if !cfg.SRE.AutomaticStart || cfg.SRE.LLM.Mode != SRELLMModeInvestigator {
		t.Fatalf("the flag changed other sre defaults: %+v", cfg.SRE)
	}
	cfg, err = loadRCAYAML(t, "sre:\n  investigator:\n    require_support_citation: true\n")
	if err != nil || !cfg.SRE.Investigator.RequireSupportCitation {
		t.Fatalf("require_support_citation: true = %+v (%v)", cfg.SRE.Investigator, err)
	}
}

func TestSRESupportCitation_OmissionKeepsItOn(t *testing.T) {
	for _, body := range []string{"sre:\n  investigator: {}\n",
		"sre:\n  llm:\n    mode: review\n", "sre: {}\n"} {
		cfg, err := loadRCAYAML(t, body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if !cfg.SRE.Investigator.RequireSupportCitation {
			t.Errorf("%q turned require_support_citation off by omission", body)
		}
	}
}

func TestSRESupportCitation_InvalidValueNamesTheKey(t *testing.T) {
	for _, bad := range []string{
		"sre:\n  investigator:\n    require_support_citation: maybe\n",
		"sre:\n  investigator:\n    require_support_citation: [true]\n",
		"sre:\n  investigator:\n    require_support_citation: 2\n",
	} {
		_, err := loadRCAYAML(t, bad)
		if err == nil {
			t.Errorf("accepted invalid value %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "sre.investigator.require_support_citation") {
			t.Errorf("%q: error %q does not name the key", bad, err)
		}
	}
}

func TestSRESupportCitation_UnknownKeyInTheBlockIsRejected(t *testing.T) {
	_, err := loadRCAYAML(t, "sre:\n  investigator:\n    require_suport_citation: false\n")
	if err == nil || !strings.Contains(err.Error(), "require_suport_citation") {
		t.Fatalf("a misspelt key was accepted or not named: %v", err)
	}
	_, err = loadRCAYAML(t, "sre:\n  investigator: [true]\n")
	if err == nil || !strings.Contains(err.Error(), "sre.investigator") {
		t.Fatalf("a non-mapping block was accepted or not named: %v", err)
	}
}

// The block is decoded by hand, so pin its tag check: only a plain YAML
// 1.2 boolean is accepted. A quoted "true", YAML 1.1 words (yes, off)
// and an empty value would each be a silent guess for a safety-critical
// switch; they fail and name the key instead.
func TestSRESupportCitation_OnlyPlainBooleansAreAccepted(t *testing.T) {
	for _, v := range []string{`"true"`, `'false'`, "yes", "off", "on", "no", "", "~"} {
		body := "sre:\n  investigator:\n    require_support_citation: " + v + "\n"
		_, err := loadRCAYAML(t, body)
		if err == nil {
			t.Errorf("value %q was accepted", v)
			continue
		}
		if !strings.Contains(err.Error(), "sre.investigator.require_support_citation") {
			t.Errorf("value %q: error %q does not name the key", v, err)
		}
	}
	for v, want := range map[string]bool{"false": false, "False": false, "TRUE": true} {
		cfg, err := loadRCAYAML(t, "sre:\n  investigator:\n    require_support_citation: "+
			v+"\n")
		if err != nil || cfg.SRE.Investigator.RequireSupportCitation != want {
			t.Errorf("value %s: got %+v (%v), want %v", v, cfg.SRE.Investigator, err, want)
		}
	}
}
