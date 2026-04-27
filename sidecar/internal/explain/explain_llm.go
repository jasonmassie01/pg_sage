package explain

import (
	"context"
	"fmt"
	"strings"

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
{"summary":"...","slow_because":["..."],"recommendations":["..."]}`

// enhanceWithLLM sends the plan to the LLM for natural language
// analysis and updates the result in place. On any error it logs
// and leaves deterministic values intact.
func (ex *Explainer) enhanceWithLLM(
	ctx context.Context, result *ExplainResult,
) {
	if ex.llmClient == nil || !ex.llmClient.IsEnabled() {
		return
	}

	maxTokens := ex.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	userMsg := fmt.Sprintf(
		"Query:\n%s\n\nEXPLAIN plan:\n%s",
		result.Query, string(result.PlanJSON),
	)

	raw, _, err := ex.llmClient.Chat(
		ctx, explainSystemPrompt, userMsg, maxTokens,
	)
	if err != nil {
		ex.logFn(
			"WARN", "explain: LLM enhancement failed: %v", err,
		)
		return
	}

	ex.applyLLMResponse(raw, result)
}

// llmExplainResponse is the expected JSON shape from the LLM.
type llmExplainResponse struct {
	Summary         string   `json:"summary"`
	SlowBecause     []string `json:"slow_because"`
	Recommendations []string `json:"recommendations"`
}

// applyLLMResponse parses the raw LLM output and updates result
// fields. On parse failure it logs and keeps the original values.
func (ex *Explainer) applyLLMResponse(
	raw string, result *ExplainResult,
) {
	var resp llmExplainResponse
	if err := llm.ParseJSON(raw, llm.JSONAuto, &resp); err != nil {
		ex.logFn(
			"WARN",
			"explain: failed to parse LLM response: %v", err,
		)
		return
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
}

// stripToJSON extracts JSON from LLM output that may contain
// thinking tokens or markdown fences. Delegates to the canonical
// llm.StripJSON with JSONAuto since this caller accepts either
// object- or array-shaped responses.
func stripToJSON(s string) string {
	return llm.StripJSON(s, llm.JSONAuto)
}

// stripMarkdownFences removes ```json ... ``` wrappers.
func stripMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
