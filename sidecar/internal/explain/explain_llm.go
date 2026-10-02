package explain

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/llm"
)

const explainSystemPrompt = `You are a PostgreSQL query ` +
	`performance analyst.
Given an EXPLAIN (ANALYZE) plan in JSON format and the original ` +
	`SQL query, provide:
1. A plain-English summary (1-2 sentences) of what the query ` +
	`does and how PostgreSQL executes it.
2. Reasons why this query may be slow (if applicable). Omit if ` +
	`the plan looks efficient.
3. Specific, actionable recommendations to improve performance ` +
	`(indexes, query rewrites, config changes). Omit if none apply.

Respond ONLY with valid JSON -- no markdown fences, no commentary:
{"summary":"...","slow_because":["..."],"recommendations":["..."]}
` + llm.UntrustedDataRule

// enhanceWithLLM sends the plan to the LLM for natural language
// analysis and updates the result in place. On any error it logs
// and leaves deterministic values intact. It reports degraded=true when
// an enabled LLM failed to answer usably (the result is a fallback).
func (ex *Explainer) enhanceWithLLM(
	ctx context.Context, result *ExplainResult,
) (degraded bool) {
	if ex.llmClient == nil || !ex.llmClient.IsEnabled() {
		return false
	}

	maxTokens := ex.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	raw, _, err := ex.llmClient.Chat(
		ctx, explainSystemPrompt, explainUserPrompt(result), maxTokens,
	)
	if err != nil {
		ex.logFn(
			"WARN", "explain: LLM enhancement failed: %v", err,
		)
		return true
	}

	return !ex.applyLLMResponse(raw, result)
}

// explainUserPrompt delimits the query and plan as untrusted data with
// literals and comments redacted: plan filters carry row values (G3-B07).
func explainUserPrompt(result *ExplainResult) string {
	return fmt.Sprintf("Query:\n%s\n\nEXPLAIN plan:\n%s",
		llm.SanitizePromptSQL("query", result.Query),
		llm.SanitizePromptSQL("plan", string(result.PlanJSON)))
}

// llmExplainResponse is the expected JSON shape from the LLM.
type llmExplainResponse struct {
	Summary         string   `json:"summary"`
	SlowBecause     []string `json:"slow_because"`
	Recommendations []string `json:"recommendations"`
}

// applyLLMResponse parses the raw LLM output and updates result
// fields. On parse failure it logs, keeps the original values and
// reports false.
func (ex *Explainer) applyLLMResponse(
	raw string, result *ExplainResult,
) bool {
	var resp llmExplainResponse
	if err := llm.ParseJSON(raw, llm.JSONAuto, &resp); err != nil {
		ex.logFn(
			"WARN",
			"explain: failed to parse LLM response: %v", err,
		)
		return false
	}

	if resp.Summary != "" {
		result.Summary = resp.Summary
	}
	if len(resp.SlowBecause) > 0 {
		result.SlowBecause = resp.SlowBecause
	}
	if len(resp.Recommendations) > 0 {
		result.Recommendations = resp.Recommendations
	}
	return true
}
