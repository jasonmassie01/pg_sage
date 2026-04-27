package advisor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/llm"
)

const maxAdvisorPromptChars = 16384

// stripToJSON extracts a JSON array from an LLM response that may
// contain thinking tokens or markdown fences. Delegates to the
// canonical llm.StripJSON implementation.
func stripToJSON(s string) string {
	return llm.StripJSON(s, llm.JSONArray)
}

// stripMarkdownFences removes ```json ... ``` wrappers from LLM
// output. Kept as a thin helper for callers that know the input
// has no JSON delimiters and only need fence removal. Uses the
// llm package's internal logic via StripJSON with an unreachable
// shape to exercise fence stripping when no JSON is present.
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

// parseLLMFindings parses the LLM JSON response into findings.
func parseLLMFindings(
	raw string,
	category string,
	logFn func(string, string, ...any),
) []analyzer.Finding {
	var recs []map[string]any
	if err := llm.ParseJSON(raw, llm.JSONArray, &recs); err != nil {
		logFn("WARN", "advisor: %s: parse error: %v", category, err)
		return nil
	}

	var findings []analyzer.Finding
	for _, rec := range recs {
		objID, _ := rec["object_identifier"].(string)
		if objID == "" {
			objID, _ = rec["table"].(string)
		}
		if objID == "" {
			objID = "instance"
		}
		severity, _ := rec["severity"].(string)
		if severity == "" {
			severity = "info"
		}
		rationale, _ := rec["rationale"].(string)
		recSQL, _ := rec["recommended_sql"].(string)

		risk := deriveActionRisk(recSQL)

		findings = append(findings, analyzer.Finding{
			Category:         category,
			Severity:         severity,
			ObjectType:       "configuration",
			ObjectIdentifier: objID,
			Title: fmt.Sprintf(
				"%s recommendation for %s", category, objID,
			),
			Detail:         rec,
			Recommendation: rationale,
			RecommendedSQL: recSQL,
			ActionRisk:     risk,
		})
	}
	return findings
}

// deriveActionRisk returns the risk level for a recommended SQL
// statement based on its type and impact. ALTER SYSTEM and DROP
// INDEX are classified as moderate since they affect production
// configuration or remove existing optimizations.
func deriveActionRisk(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(upper, "ALTER SYSTEM"):
		return "moderate"
	case strings.HasPrefix(upper, "DROP INDEX"):
		return "moderate"
	default:
		return "safe"
	}
}
