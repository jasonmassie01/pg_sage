package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-05a: the gate judges the model the review pass would run on. No
// concurrent access test: reviewModelName is a pure function of the config.

func TestReviewModelName_FollowsTheReviewClientChoice(t *testing.T) {
	base := func() *config.Config {
		c := config.DefaultConfig()
		c.LLM.Enabled, c.LLM.Model = true, "general-1"
		return c
	}
	off := base()
	off.LLM.Enabled = false
	review := base()
	review.LLM.ReviewLLM.Enabled, review.LLM.ReviewLLM.Model = true, "review-1"
	inherit := base()
	inherit.LLM.ReviewLLM.Enabled = true
	opt := base()
	opt.LLM.OptimizerLLM.Enabled, opt.LLM.OptimizerLLM.Model = true, "opt-1"
	for name, c := range map[string]struct {
		cfg  *config.Config
		want string
	}{
		"nil config":           {nil, ""},
		"llm off":              {off, ""},
		"general":              {base(), "general-1"},
		"review client":        {review, "review-1"},
		"review inherits":      {inherit, "general-1"},
		"optimizer when no rv": {opt, "opt-1"},
	} {
		if got := reviewModelName(c.cfg); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestEvaluateReviewGate_WithoutAPoolDoesNothing(t *testing.T) {
	logs := captureLogs(t)
	rt := &databaseRuntime{cfg: config.DefaultConfig()}
	rt.evaluateReviewGate(context.Background()) // must not panic or touch a database
	if out := logs.String(); strings.Contains(out, "review model gate") {
		t.Fatalf("a runtime without a pool evaluated the gate: %q", out)
	}
}
