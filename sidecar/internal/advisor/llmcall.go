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
// truncated on a UTF-8/block boundary and delimited as untrusted data;
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
	user := llm.UntrustedData(label, truncateAdvisorPrompt(data))
	resp, _, err := mgr.ChatForPurpose(ctx, "advisor", sys, user, advisorMaxTokens)
	if err != nil {
		return "", fmt.Errorf("%s LLM: %w", label, err)
	}
	return resp, nil
}

// truncateAdvisorPrompt caps a prompt at maxAdvisorPromptChars bytes
// without splitting a UTF-8 sequence, preferring to cut at the last
// blank-line (per-object context) boundary in the second half.
func truncateAdvisorPrompt(prompt string) string {
	if len(prompt) <= maxAdvisorPromptChars {
		return prompt
	}
	cut := maxAdvisorPromptChars
	for cut > 0 && !utf8.RuneStart(prompt[cut]) {
		cut--
	}
	head := prompt[:cut]
	if i := strings.LastIndex(head, "\n\n"); i >= maxAdvisorPromptChars/2 {
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
