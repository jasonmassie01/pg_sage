package modellift

import (
	"strings"
	"testing"
)

// llm-01: model-root authority belongs to the model that earned it. A
// held-out measurement taken with one model never grants override
// authority to another: the rule compares the measured model with the
// investigator's running model by exact normalized ID.

// No concurrent access tests: NormalizeModel and OverrideRule are pure
// functions of their arguments.

// measuredWith is eligible evidence measured with measured, asked for an
// investigator running running.
func measuredWith(measured, running string) Evidence {
	e := eligible(40, 40)
	e.Model, e.RunningModel = measured, running
	return e
}

func TestNormalizeModel(t *testing.T) {
	for in, want := range map[string]string{
		"gpt-6":                   "gpt-6",
		"OpenAI/GPT-6":            "gpt-6",
		"gpt-6 ":                  "gpt-6",
		"\t GPT-6\n":              "gpt-6",
		"openrouter/openai/gpt-6": "gpt-6",
		"openai/ gpt-6 ":          "gpt-6",
		"gpt-6-luna":              "gpt-6-luna",
		"llama3.1:8b":             "llama3.1:8b",
		"":                        "",
		"   ":                     "",
		"openai/":                 "",
		"/":                       "",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverrideRule_MeasuredModelMustBeTheRunningOne(t *testing.T) {
	if v := OverrideRule(measuredWith("gpt-6", "gpt-6")); !v.Eligible {
		t.Fatalf("gpt-6 measured, gpt-6 running: %+v, want adopt", v)
	}
	v := OverrideRule(measuredWith("gpt-6", "gpt-6-luna"))
	if v.Eligible {
		t.Fatalf("gpt-6 measured, gpt-6-luna running: eligible, want advisory")
	}
	want := `measured with "gpt-6"; the investigator runs "gpt-6-luna"`
	if !strings.Contains(v.Reason, want) {
		t.Fatalf("reason %q, want it to contain %q", v.Reason, want)
	}
	if v.LowerBound == nil || *v.LowerBound < MinOverrideLowerBound {
		t.Fatalf("a model mismatch must still show the measured lower bound: %v",
			v.LowerBound)
	}
}

func TestOverrideRule_NormalizedIDsMatch(t *testing.T) {
	for _, c := range [][2]string{
		{"OpenAI/GPT-6", "gpt-6"},
		{"gpt-6 ", "gpt-6"},
		{"gpt-6", "openai/gpt-6"},
		{"GPT-6", " gpt-6\t"},
	} {
		if v := OverrideRule(measuredWith(c[0], c[1])); !v.Eligible {
			t.Errorf("measured %q, running %q: %q, want adopt", c[0], c[1], v.Reason)
		}
	}
}

func TestOverrideRule_NearMissModelIDsDoNotMatch(t *testing.T) {
	// Family sharing is deferred: only the exact normalized ID counts.
	for _, running := range []string{"gpt-60", "gpt-6-mini", "gpt-", "gpt", "gpt-6.1"} {
		v := OverrideRule(measuredWith("gpt-6", running))
		if v.Eligible || !strings.Contains(v.Reason, "the investigator runs") {
			t.Errorf("running %q: %+v, want a mismatch", running, v)
		}
	}
}

func TestOverrideRule_UnknownModelsAreAdvisory(t *testing.T) {
	for name, c := range map[string]struct {
		measured, running, reason string
	}{
		"no measured model":   {"", "gpt-6", "the measurement names no model"},
		"blank measured":      {"   ", "gpt-6", "the measurement names no model"},
		"prefix only":         {"openai/", "gpt-6", "the measurement names no model"},
		"no running model":    {"gpt-6", "", "the investigator's model is unknown"},
		"blank running model": {"gpt-6", " \t", "the investigator's model is unknown"},
		"neither":             {"", "", "the measurement names no model"},
	} {
		v := OverrideRule(measuredWith(c.measured, c.running))
		if v.Eligible {
			t.Errorf("%s: eligible, want advisory", name)
		}
		if !strings.Contains(v.Reason, c.reason) {
			t.Errorf("%s: reason %q, want %q", name, v.Reason, c.reason)
		}
	}
}

func TestOverrideRule_ModelCheckDoesNotHideEarlierReasons(t *testing.T) {
	e := measuredWith("", "")
	e.Mode = "fake"
	if v := OverrideRule(e); !strings.Contains(v.Reason, "live model") {
		t.Fatalf("a fake-model run must say so before the model identity: %q", v.Reason)
	}
	e = measuredWith("gpt-6", "gpt-6")
	e.Split = SplitTuning
	if v := OverrideRule(e); !strings.Contains(v.Reason, "held-out") {
		t.Fatalf("the split is checked first: %q", v.Reason)
	}
}

func TestOverrideRule_MatchingModelKeepsTheOtherConditions(t *testing.T) {
	// The Wilson threshold and the minimum override count are unchanged.
	e := measuredWith("gpt-6", "gpt-6")
	e.Overrides = Proportion{K: 15, N: 15}
	if v := OverrideRule(e); v.Eligible || !strings.Contains(v.Reason, "below 0.80") {
		t.Fatalf("15/15 with a matching model: %+v, want below the bound", v)
	}
	e.Overrides = Proportion{K: 9, N: 9}
	if v := OverrideRule(e); v.Eligible || !strings.Contains(v.Reason, "fewer than") {
		t.Fatalf("9/9 with a matching model: %+v, want too few", v)
	}
	e = measuredWith("gpt-6", "gpt-6")
	e.Forbidden = 1
	if v := OverrideRule(e); v.Eligible || !strings.Contains(v.Reason, "forbidden") {
		t.Fatalf("forbidden action with a matching model: %+v", v)
	}
}
