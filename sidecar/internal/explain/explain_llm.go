package explain

import (
	"context"
	"fmt"
	"unicode/utf8"

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

	user := explainUserPrompt(result, ex.llmClient.ContextBudgetBytes())
	pctx := llm.WithPurpose(ctx, "explain") // llm-04: usage ledger label
	raw, _, err := ex.llmClient.Chat(pctx, explainSystemPrompt, user, maxTokens)
	if err != nil {
		ex.logFn(
			"WARN", "explain: LLM enhancement failed: %v", err,
		)
		return true
	}

	return !ex.applyLLMResponse(raw, result)
}

// maxExplainPlanBytes is explain's own cap on the plan text it sends.
const maxExplainPlanBytes = 65536

// explainPlanLimit is the plan's byte cap: maxExplainPlanBytes, or the
// context budget (llm.context_budget_tokens in bytes) when that is
// smaller. A budget of 0 adds no trim (llm-05).
func explainPlanLimit(budgetBytes int) int {
	if budgetBytes > 0 && budgetBytes < maxExplainPlanBytes {
		return budgetBytes
	}
	return maxExplainPlanBytes
}

// explainUserPrompt delimits the query and plan as untrusted data with
// literals and comments redacted: plan filters carry row values (G3-B07).
// The plan is clipped after redaction, so a cut never exposes a literal.
func explainUserPrompt(result *ExplainResult, budgetBytes int) string {
	plan := clipPlan(llm.SanitizeForLLM(string(result.PlanJSON)),
		explainPlanLimit(budgetBytes))
	return fmt.Sprintf("Query:\n%s\n\nEXPLAIN plan:\n%s",
		llm.SanitizePromptSQL("query", result.Query),
		llm.UntrustedData("plan", plan))
}

// clipPlan cuts plan to limit bytes on a rune boundary and says so.
func clipPlan(plan string, limit int) string {
	if len(plan) <= limit {
		return plan
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(plan[cut]) {
		cut--
	}
	return plan[:cut] + planClipNote(cut, len(plan))
}

// planClipNote tells the model the plan JSON was cut.
func planClipNote(kept, total int) string {
	return fmt.Sprintf("\n... plan truncated: %d of %d bytes kept", kept, total)
}

// llmExplainResponse is the expected JSON shape from the LLM.
type llmExplainResponse struct {
	Summary         string   `json:"summary"`
	SlowBecause     []string `json:"slow_because"`
	Recommendations []string `json:"recommendations"`
}

// applyLLMResponse parses the raw LLM output: a non-empty summary replaces
// the rules summary, and its entries are added after the rule entries,
// tagged llm and deduplicated; a rule entry is never removed (tuning-10).
// On parse failure it logs, keeps the original values and reports false.
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
	for _, reason := range resp.SlowBecause {
		result.addEntry(InsightSlowBecause, reason, SourceLLM, "")
	}
	for _, rec := range resp.Recommendations {
		result.addEntry(InsightRecommendation, rec, SourceLLM, "")
	}
	return true
}
