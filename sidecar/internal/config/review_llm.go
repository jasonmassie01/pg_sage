package config

import "strings"

// ReviewLLMConfig configures the dedicated model of Sage Review's model
// pass (review-03a), the same shape as OptimizerLLMConfig. When it is not
// enabled the review uses the optimizer client, else the general one.
type ReviewLLMConfig struct {
	Enabled           bool   `yaml:"enabled" doc:"Use a dedicated model for Sage Review's model pass. When false the review uses the optimizer model when enabled, else the general llm.* client. Default: false."`
	Endpoint          string `yaml:"endpoint" doc:"Base URL of the review model's provider; empty uses llm.endpoint. The general API key is sent only to the same origin."`
	APIKey            string `yaml:"api_key" doc:"API key of the review model. Prefer SAGE_REVIEW_LLM_API_KEY or its _FILE form over a literal value." secret:"true"`
	Model             string `yaml:"model" doc:"Review model identifier; empty uses llm.model. Must support tool calls or JSON replies."`
	TimeoutSeconds    int    `yaml:"timeout_seconds" doc:"Per-request timeout of review model calls; 0 uses llm.timeout_seconds."`
	TokenBudgetDaily  int    `yaml:"token_budget_daily" doc:"Daily token cap of the review model, separate from llm.token_budget_daily; 0 uses llm.token_budget_daily."`
	CooldownSeconds   int    `yaml:"cooldown_seconds" doc:"As llm.cooldown_seconds, for the review client; 0 uses llm.cooldown_seconds."`
	MaxOutputTokens   int    `yaml:"max_output_tokens" doc:"Upper bound on output tokens accepted from the review model per call; 0 leaves the review's own step cap."`
	FallbackToGeneral bool   `yaml:"fallback_to_general" doc:"Fall back to the general llm.* client when the review model is unavailable or over budget. Default: true."`
}

// ReviewAuth is the review client's auth mode, by OptimizerAuth's rule: a
// key or endpoint of its own is api_key auth, else the general mode.
func (l LLMConfig) ReviewAuth() string {
	if DatabricksAppMode() && l.Auth == "databricks" && l.APIKey == "" &&
		l.ReviewLLM.APIKey == "" {
		return "databricks"
	}
	if l.ReviewLLM.APIKey != "" || (l.ReviewLLM.Endpoint != "" &&
		strings.TrimRight(l.ReviewLLM.Endpoint, "/") != strings.TrimRight(l.Endpoint, "/")) {
		return "api_key"
	}
	return l.Auth
}
