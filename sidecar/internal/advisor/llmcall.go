package advisor

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const advisorMaxTokens = 4096

// maxSharedBuffersFraction caps a shared_buffers recommendation as a
// fraction of known host RAM (the PG docs' "beyond ~40%" guidance).
const maxSharedBuffersFraction = 0.40

// chatAdvisor sends one sub-advisor prompt. The DB-derived context is
// truncated on a UTF-8/block boundary (to llm.context_budget_tokens when
// that is below the advisor's own cap) and delimited as untrusted data;
// the system prompt carries the rule to never follow instructions inside
// it (G3-B07, G3-B26). trusted is code-authored grounding (e.g. the GUC
// documentation prior) appended to the system prompt, outside the data.
func chatAdvisor(
	ctx context.Context,
	mgr *llm.Manager,
	label, system, trusted, data string,
) (string, error) {
	sys := system
	if trusted != "" {
		sys += "\n\n" + trusted
	}
	sys += "\n\n" + llm.UntrustedDataRule
	budget := mgr.ForPurpose("advisor").ContextBudgetBytes()
	user := llm.UntrustedData(label, truncateAdvisorPrompt(data, budget))
	if facts := promptFactsFrom(ctx); facts != "" {
		user += "\n\nOperator-confirmed facts about this database (binding: respect " +
			"them):\n" + llm.UntrustedData("confirmed_facts", facts)
	}
	resp, _, err := mgr.ChatForPurpose(ctx, "advisor", sys, user, advisorMaxTokens)
	if err != nil {
		return "", fmt.Errorf("%s LLM: %w", label, err)
	}
	return resp, nil
}

// advisorPromptLimit is the byte cap of the advisor's data:
// maxAdvisorPromptChars, or the context budget (llm.context_budget_tokens
// in bytes) when that is smaller. A budget of 0 adds no trim (llm-05).
func advisorPromptLimit(budgetBytes int) int {
	if budgetBytes > 0 && budgetBytes < maxAdvisorPromptChars {
		return budgetBytes
	}
	return maxAdvisorPromptChars
}

// truncateAdvisorPrompt caps a prompt at advisorPromptLimit(budgetBytes)
// bytes without splitting a UTF-8 sequence, preferring to cut at the last
// blank-line (per-object context) boundary in the second half.
func truncateAdvisorPrompt(prompt string, budgetBytes int) string {
	limit := advisorPromptLimit(budgetBytes)
	if len(prompt) <= limit {
		return prompt
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(prompt[cut]) {
		cut--
	}
	head := prompt[:cut]
	if i := strings.LastIndex(head, "\n\n"); i >= limit/2 {
		head = head[:i]
	}
	return head
}

// applyHostMemoryGuard refuses executable shared_buffers changes unless
// host RAM is known and the value stays within maxSharedBuffersFraction
// of it (G3-B08). shared_buffers is restart-required and PostgreSQL
// exposes no RAM figure, so an ungrounded value can prevent the next
// postmaster start. The finding is kept as advisory (info, no SQL).
func applyHostMemoryGuard(
	findings []analyzer.Finding, hostMemBytes int64,
) []analyzer.Finding {
	for i := range findings {
		stmt, ok := pgconf.ParseAlterSystem(findings[i].RecommendedSQL)
		if !ok || stmt.Reset || stmt.Name != "shared_buffers" {
			continue
		}
		reason := sharedBuffersRefusal(stmt.Value, hostMemBytes)
		if reason == "" {
			continue
		}
		findings[i].RecommendedSQL = ""
		findings[i].RollbackSQL = ""
		findings[i].ActionRisk = ""
		findings[i].Severity = "info"
		findings[i].Recommendation += " (Not executable: " + reason + ".)"
	}
	return findings
}

func sharedBuffersRefusal(value string, hostMemBytes int64) string {
	if hostMemBytes <= 0 {
		return "host memory is unknown, so shared_buffers cannot be " +
			"grounded; review manually"
	}
	v, err := pgconf.ParseValue(value, pgconf.Docs["shared_buffers"])
	if err != nil {
		return "unparseable shared_buffers value"
	}
	if v > float64(hostMemBytes)*maxSharedBuffersFraction {
		return fmt.Sprintf("shared_buffers exceeds %.0f%% of host memory",
			maxSharedBuffersFraction*100)
	}
	return ""
}
