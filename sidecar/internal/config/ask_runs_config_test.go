package config

import (
	"strings"
	"testing"
)

// ask.max_concurrent_runs (mcp-07b): asynchronous Ask Sage runs at once
// per database on one sidecar, 1-16, default 4.
// No concurrent access tests: validation is a pure function of the value.

func TestAskMaxConcurrentRuns_DefaultWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if DefaultAskMaxConcurrentRuns != 4 {
		t.Fatalf("DefaultAskMaxConcurrentRuns = %d, want 4", DefaultAskMaxConcurrentRuns)
	}
	for name, got := range map[string]int{"Load": cfg.Ask.MaxConcurrentRuns,
		"DefaultConfig": DefaultConfig().Ask.MaxConcurrentRuns} {
		if got != 4 {
			t.Errorf("%s ask.max_concurrent_runs = %d, want 4", name, got)
		}
	}
}

func TestAskMaxConcurrentRuns_PartialSectionKeepsTheDefault(t *testing.T) {
	c, err := loadRCAYAML(t, "ask:\n  max_seconds_per_question: 90\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Ask.MaxConcurrentRuns != 4 || c.Ask.MaxSecondsPerQuestion != 90 {
		t.Fatalf("ask = %+v", c.Ask)
	}
}

func TestAskMaxConcurrentRuns_Boundaries(t *testing.T) {
	for v, want := range map[string]int{"1": 1, "16": 16, "8": 8} {
		c, err := loadRCAYAML(t, "ask:\n  max_concurrent_runs: "+v+"\n")
		if err != nil {
			t.Errorf("%s refused: %v", v, err)
			continue
		}
		if c.Ask.MaxConcurrentRuns != want {
			t.Errorf("%s: got %d, want %d", v, c.Ask.MaxConcurrentRuns, want)
		}
	}
	for _, v := range []string{"0", "17", "-1"} {
		_, err := loadRCAYAML(t, "ask:\n  max_concurrent_runs: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "ask.max_concurrent_runs") ||
			!strings.Contains(err.Error(), "1-16") {
			t.Errorf("%s: error %v does not name the key and its range", v, err)
		}
	}
}

func TestAskMaxConcurrentRuns_ValidateRefusesOutOfRange(t *testing.T) {
	for v, ok := range map[int]bool{0: false, 1: true, 16: true, 17: false} {
		a := defaultAskConfig()
		a.MaxConcurrentRuns = v
		err := a.validate()
		if ok != (err == nil) {
			t.Errorf("validate(%d) = %v, want ok=%v", v, err, ok)
		}
	}
}

func TestAskMaxConcurrentRuns_DocTagLifecycleAndClass(t *testing.T) {
	doc := collectDocTags(t, DefaultConfig())["ask.max_concurrent_runs"]
	for _, fragment := range []string{"Default: 4", "1-16"} {
		if !strings.Contains(doc, fragment) {
			t.Errorf("ask.max_concurrent_runs doc %q lacks %q", doc, fragment)
		}
	}
	lc, ok := LookupFieldLifecycle("ask.max_concurrent_runs")
	if !ok || lc.Lifecycle != LifecycleRestart {
		t.Errorf("lifecycle = %+v (%v), want restart", lc, ok)
	}
	if class, ok := KeyClassOf("ask.max_concurrent_runs"); !ok ||
		class != KeyOperatorPreference {
		t.Errorf("class = %q (%v), want operator_preference", class, ok)
	}
}
