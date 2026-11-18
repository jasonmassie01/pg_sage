package config

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/appmode"
)

// review.model_pass, review.model_runs, review.consensus,
// review.hash_identifiers and llm.review_llm (review-03a): the model review
// pass is on whenever a model is usable, runs three times and keeps what a
// majority keeps.
// No concurrent access tests: loading and validation are pure functions of
// the file and the environment.

func TestReviewModelPass_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range []ReviewConfig{cfg.Review, DefaultConfig().Review} {
		if r.ModelPass != "auto" || r.ModelRuns != 3 || r.Consensus != "majority" ||
			r.HashIdentifiers {
			t.Fatalf("review = %+v, want model_pass auto, 3 runs, majority, no hashing", r)
		}
	}
	rl := cfg.LLM.ReviewLLM
	if rl.Enabled || !rl.FallbackToGeneral || rl.APIKey != "" || rl.Endpoint != "" {
		t.Fatalf("review_llm = %+v, want off, falling back to the general client", rl)
	}
}

func TestReviewModelPass_PartialSectionKeepsTheDefaults(t *testing.T) {
	c, err := loadRCAYAML(t, "review:\n  window_days: 30\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Review.ModelPass != "auto" || c.Review.ModelRuns != 3 ||
		c.Review.Consensus != "majority" {
		t.Fatalf("review = %+v, want the model-pass defaults", c.Review)
	}
}

func TestReviewModelPass_AcceptsEveryMode(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		c, err := loadRCAYAML(t, "review:\n  model_pass: "+mode+"\n")
		if err != nil || c.Review.ModelPass != mode {
			t.Errorf("%s: %+v %v", mode, c.Review, err)
		}
	}
	c, err := loadRCAYAML(t, "review:\n  consensus: refute\n  hash_identifiers: true\n")
	if err != nil || c.Review.Consensus != "refute" || !c.Review.HashIdentifiers {
		t.Errorf("refute: %+v %v", c.Review, err)
	}
}

func TestReviewModelPass_RefusesUnknownWords(t *testing.T) {
	cases := map[string]string{
		"review:\n  model_pass: maybe\n":   "review.model_pass",
		"review:\n  model_pass: \"\"\n":    "review.model_pass",
		"review:\n  consensus: vote\n":     "review.consensus",
		"review:\n  consensus: MAJORITY\n": "review.consensus",
	}
	for body, key := range cases {
		_, err := loadRCAYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: error %v does not name %s", body, err, key)
		}
	}
}

func TestReviewModelRuns_OddOneToFive(t *testing.T) {
	for _, v := range []string{"1", "3", "5"} {
		c, err := loadRCAYAML(t, "review:\n  model_runs: "+v+"\n")
		if err != nil || c.Review.ModelRuns != int(v[0]-'0') {
			t.Errorf("%s: %+v %v", v, c.Review, err)
		}
	}
	for _, v := range []string{"0", "2", "4", "6", "7", "-1"} {
		_, err := loadRCAYAML(t, "review:\n  model_runs: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "review.model_runs") ||
			!strings.Contains(err.Error(), "odd") || !strings.Contains(err.Error(), "1-5") {
			t.Errorf("%s: error %v does not name the key, odd and 1-5", v, err)
		}
	}
}

func TestReviewModelPass_DocTagsAndClasses(t *testing.T) {
	tags := collectDocTags(t, DefaultConfig())
	want := map[string][]string{
		"review.model_pass":                  {"Default: auto", "off"},
		"review.model_runs":                  {"Default: 3", "1-5"},
		"review.consensus":                   {"Default: majority", "refute"},
		"review.hash_identifiers":            {"Default: false"},
		"llm.review_llm.enabled":             {"Default: false"},
		"llm.review_llm.fallback_to_general": {"Default: true"},
	}
	for key, fragments := range want {
		for _, f := range fragments {
			if !strings.Contains(tags[key], f) {
				t.Errorf("doc of %s = %q lacks %q", key, tags[key], f)
			}
		}
		if _, ok := KeyClassOf(key); !ok {
			t.Errorf("%s has no key class", key)
		}
	}
}

func TestReviewLLM_SectionAndEnvironment(t *testing.T) {
	t.Setenv("SAGE_REVIEW_LLM_API_KEY", "review-env-key")
	c, err := loadRCAYAML(t, "llm:\n  review_llm:\n    enabled: true\n"+
		"    endpoint: https://review.example/v1\n    model: big-model\n"+
		"    fallback_to_general: false\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rl := c.LLM.ReviewLLM
	if !rl.Enabled || rl.Endpoint != "https://review.example/v1" || rl.Model != "big-model" ||
		rl.FallbackToGeneral || rl.APIKey != "review-env-key" {
		t.Fatalf("review_llm = %+v (key redacted in check)", redactedReviewLLM(rl))
	}
}

func TestReviewLLM_DatabricksAppForbidsItsKeyAndForeignEndpoint(t *testing.T) {
	t.Setenv(appmode.AuthModeEnv, appmode.AuthMode)
	t.Setenv("DATABRICKS_HOST", "https://ws.example")
	if !DatabricksAppMode() {
		t.Fatal("app mode is not detected from its auth mode variable")
	}
	l := LLMConfig{Auth: "databricks"}
	l.ReviewLLM.APIKey = "k"
	if err := l.ValidateDatabricksApp(); err == nil ||
		!strings.Contains(err.Error(), "review_llm") {
		t.Errorf("review key in app mode: %v, want a refusal naming review_llm", err)
	}
	l.ReviewLLM.APIKey = ""
	l.ReviewLLM.Endpoint = "https://elsewhere.example/v1"
	if err := l.ValidateDatabricksApp(); err == nil ||
		!strings.Contains(err.Error(), "llm.review_llm.endpoint") {
		t.Errorf("foreign review endpoint in app mode: %v, want a refusal", err)
	}
}

func redactedReviewLLM(r ReviewLLMConfig) ReviewLLMConfig {
	if r.APIKey != "" {
		r.APIKey = "<set>"
	}
	return r
}
