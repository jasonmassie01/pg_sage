package config

import (
	"strings"
	"testing"
)

// ask.max_seconds_per_question (mcp-07): the wall clock of one question,
// 15-300 s, default 60. The API gives the ask route this plus 5 s.
// No concurrent access tests: validation is a pure function of the value.

func TestAskMaxSeconds_DefaultWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if DefaultAskMaxSecondsPerQuestion != 60 {
		t.Fatalf("DefaultAskMaxSecondsPerQuestion = %d, want 60",
			DefaultAskMaxSecondsPerQuestion)
	}
	for name, got := range map[string]int{"Load": cfg.Ask.MaxSecondsPerQuestion,
		"DefaultConfig": DefaultConfig().Ask.MaxSecondsPerQuestion} {
		if got != 60 {
			t.Errorf("%s ask.max_seconds_per_question = %d, want 60", name, got)
		}
	}
}

func TestAskMaxSeconds_PartialSectionKeepsTheDefault(t *testing.T) {
	c, err := loadRCAYAML(t, "ask:\n  retention_days: 7\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Ask.MaxSecondsPerQuestion != 60 || c.Ask.RetentionDays != 7 {
		t.Fatalf("ask = %+v", c.Ask)
	}
}

func TestAskMaxSeconds_Boundaries(t *testing.T) {
	for body, want := range map[string]int{
		"ask:\n  max_seconds_per_question: 15\n":  15,
		"ask:\n  max_seconds_per_question: 300\n": 300,
		"ask:\n  max_seconds_per_question: 40\n":  40,
	} {
		c, err := loadRCAYAML(t, body)
		if err != nil {
			t.Errorf("%q refused: %v", body, err)
			continue
		}
		if c.Ask.MaxSecondsPerQuestion != want {
			t.Errorf("%q: got %d, want %d", body, c.Ask.MaxSecondsPerQuestion, want)
		}
	}
	for _, v := range []string{"14", "301", "0", "-1"} {
		body := "ask:\n  max_seconds_per_question: " + v + "\n"
		_, err := loadRCAYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), "ask.max_seconds_per_question") ||
			!strings.Contains(err.Error(), "15-300") {
			t.Errorf("%q: error %v does not name the key and its range", body, err)
		}
	}
}

func TestAskMaxSeconds_ValidateRefusesOutOfRange(t *testing.T) {
	for v, ok := range map[int]bool{14: false, 15: true, 300: true, 301: false} {
		a := defaultAskConfig()
		a.MaxSecondsPerQuestion = v
		err := a.validate()
		if ok != (err == nil) {
			t.Errorf("validate(%d) = %v, want ok=%v", v, err, ok)
		}
	}
	// A disabled Ask Sage still refuses a nonsense bound: it is checked
	// when the operator turns Ask Sage back on without editing it.
	a := defaultAskConfig()
	a.Enabled, a.MaxSecondsPerQuestion = false, 301
	if err := a.validate(); err == nil {
		t.Error("a disabled Ask Sage accepted max_seconds_per_question 301")
	}
}

func TestAskMaxSeconds_DocTagLifecycleAndClass(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	doc := docs["ask.max_seconds_per_question"]
	for _, fragment := range []string{"Default: 60", "15-300"} {
		if !strings.Contains(doc, fragment) {
			t.Errorf("ask.max_seconds_per_question doc %q lacks %q", doc, fragment)
		}
	}
	lc, ok := LookupFieldLifecycle("ask.max_seconds_per_question")
	if !ok || lc.Lifecycle != LifecycleRestart {
		t.Errorf("lifecycle = %+v (%v), want restart", lc, ok)
	}
	if class, ok := KeyClassOf("ask.max_seconds_per_question"); !ok ||
		class != KeyOperatorPreference {
		t.Errorf("class = %q (%v), want operator_preference", class, ok)
	}
}
