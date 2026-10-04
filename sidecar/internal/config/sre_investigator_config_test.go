package config

import (
	"strings"
	"testing"
)

// Roadmap 2.1: with an LLM configured, investigations use the
// tool-calling investigator by default (sre.llm.mode: investigator); the
// M3 single review turn stays available as mode "review". An absent
// file, a partial section and an empty sre.llm block keep the default.

func TestSRELLMMode_DefaultsToTheInvestigator(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SRE.LLM.Mode != SRELLMModeInvestigator ||
		DefaultConfig().SRE.LLM.Mode != SRELLMModeInvestigator {
		t.Fatalf("sre.llm.mode default = %q, want %q", cfg.SRE.LLM.Mode,
			SRELLMModeInvestigator)
	}
	for _, body := range []string{"sre:\n  automatic_start: true\n", "sre:\n  llm: {}\n",
		"sre:\n  llm:\n    enabled: true\n"} {
		c, err := loadRCAYAML(t, body)
		if err != nil || c.SRE.LLM.Mode != SRELLMModeInvestigator {
			t.Errorf("%q: mode = %q (%v), want the investigator", body, c.SRE.LLM.Mode, err)
		}
	}
}

func TestSRELLMMode_ExplicitValues(t *testing.T) {
	for _, mode := range []string{SRELLMModeInvestigator, SRELLMModeReview} {
		c, err := loadRCAYAML(t, "sre:\n  llm:\n    mode: "+mode+"\n")
		if err != nil || c.SRE.LLM.Mode != mode {
			t.Errorf("mode %s = %q (%v)", mode, c.SRE.LLM.Mode, err)
		}
	}
	for _, bad := range []string{`""`, "agent", "INVESTIGATOR", "review,investigator"} {
		_, err := loadRCAYAML(t, "sre:\n  llm:\n    mode: "+bad+"\n")
		if err == nil || !strings.Contains(err.Error(), "sre.llm.mode") {
			t.Errorf("mode %s accepted or unnamed in the error: %v", bad, err)
		}
	}
}

func TestSRELLMMode_DocTagNamesBothModes(t *testing.T) {
	doc := collectDocTags(t, DefaultConfig())["sre.llm.mode"]
	for _, want := range []string{"investigator", "review", "Default: investigator"} {
		if !strings.Contains(doc, want) {
			t.Errorf("sre.llm.mode doc %q lacks %q", doc, want)
		}
	}
}
