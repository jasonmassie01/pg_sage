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

		// A single recommendation may carry several config statements (e.g.
		// WAL tuning sets checkpoint_timeout, max_wal_size, min_wal_size).
		// Split them so each is an atomic, individually validated and
		// reloadable action — the executor rejects multi-statement SQL. A
		// single statement keeps its original shape (no behavior change).
		stmts := splitSQLStatements(recSQL)
		multi := len(stmts) > 1
		if !multi {
			stmts = []string{recSQL}
		}
		for _, stmt := range stmts {
			// Doc-grounded validation (A3): drop any ALTER SYSTEM value the
			// LLM proposed that falls outside the documented safe range.
			if ok, reason := ValidateConfigSQL(stmt); !ok {
				if logFn != nil {
					logFn("advisor", "rejecting recommendation: %s", reason)
				}
				continue
			}

			oid := objID
			title := fmt.Sprintf("%s recommendation for %s", category, objID)
			if multi {
				if param, _, ok := parseAlterSystemSet(stmt); ok {
					oid = objID + ":" + param
					title = fmt.Sprintf("%s: set %s", category, param)
				}
			}

			findings = append(findings, analyzer.Finding{
				Category:         category,
				Severity:         severity,
				ObjectType:       "configuration",
				ObjectIdentifier: oid,
				Title:            title,
				Detail:           rec,
				Recommendation:   rationale,
				RecommendedSQL:   stmt,
				ActionRisk:       deriveActionRisk(stmt),
			})
		}
	}
	return findings
}

// splitSQLStatements splits a recommendation into individual statements,
// preserving a single statement unchanged. Each returned statement ends
// with a semicolon so it is a complete, single-statement action.
func splitSQLStatements(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s+";")
		}
	}
	return out
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
