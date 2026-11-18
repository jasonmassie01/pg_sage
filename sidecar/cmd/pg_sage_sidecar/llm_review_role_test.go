package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-03a: llm.review_llm derives the review client from the general
// llm config the way optimizer_llm does.
// No concurrent access tests: derivation is a pure function of its input.

func TestReviewLLMConfig_InheritsEmptyFields(t *testing.T) {
	parent := config.LLMConfig{Enabled: true, Endpoint: "https://general.example/v1",
		APIKey: "general-key", Model: "g", TimeoutSeconds: 30, TokenBudgetDaily: 1000,
		CooldownSeconds: 60, JSONMode: true}
	parent.ReviewLLM = config.ReviewLLMConfig{Enabled: true, Model: "big"}
	got := derivedLLMConfig(parent, llmRoleReview, true)
	if !got.Enabled || got.Model != "big" || got.Endpoint != parent.Endpoint ||
		got.APIKey != "general-key" || got.TimeoutSeconds != 30 ||
		got.TokenBudgetDaily != 1000 || got.CooldownSeconds != 60 || !got.JSONMode {
		t.Fatalf("derived = %+v, want the review model over the general settings",
			redactKey(got))
	}
}

func TestReviewLLMConfig_KillSwitchesAndForeignOrigin(t *testing.T) {
	parent := config.LLMConfig{Enabled: false, Endpoint: "https://general.example/v1",
		APIKey: "general-key"}
	parent.ReviewLLM = config.ReviewLLMConfig{Enabled: true,
		Endpoint: "https://other.example/v1"}
	got := derivedLLMConfig(parent, llmRoleReview, true)
	if got.Enabled {
		t.Fatal("llm.enabled=false did not disable the review client")
	}
	if got.APIKey != "" || got.Auth != "api_key" {
		t.Fatalf("foreign review origin got a key %v, auth %q; want none and api_key",
			got.APIKey != "", got.Auth)
	}
	parent.Enabled = true
	if derivedLLMConfig(parent, llmRoleReview, false).Enabled {
		t.Fatal("a database with llm_enabled=false got an enabled review client")
	}
}

func TestReviewRole_LabelsEveryWhere(t *testing.T) {
	if llmRoleReview.String() != "review" || llmUsageRole(llmRoleReview) != "review" ||
		llmStatusKey(llmRegistryEntry{role: llmRoleReview, database: "db"}) != "db/review" {
		t.Fatal("the review role is not labelled review")
	}
	if llmUsageRole(llmRoleOptimizer) != "optimizer" || llmUsageRole(llmRoleGeneral) != "general" {
		t.Fatal("the existing roles changed their labels")
	}
}

func redactKey(c config.LLMConfig) config.LLMConfig {
	if c.APIKey != "" {
		c.APIKey = "<set>"
	}
	return c
}
