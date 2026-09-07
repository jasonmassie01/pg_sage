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
		if len(stmts) == 0 {
			if rawStatementCount(recSQL) > 0 {
				// recSQL was non-empty but only an apply mechanism
				// (e.g. a bare reload) — nothing actionable to record.
				continue
			}
			// Empty/null recommended_sql — preserve the original
			// behavior: one advisory finding carrying no SQL.
			stmts = []string{recSQL}
		} else if !multi && rawStatementCount(recSQL) == 1 {
			// Single statement to begin with — keep its verbatim shape.
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
//
// Config-apply mechanism statements (e.g. SELECT pg_reload_conf()) are
// dropped: they are not independent recommendations, and the executor
// already issues the reload itself after an ALTER SYSTEM via config_apply.
// Emitting one as its own finding would create an orphan action that
// reloads nothing meaningful on its own.
func splitSQLStatements(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, ";") {
		if s = strings.TrimSpace(s); s != "" && !isApplyMechanism(s) {
			out = append(out, s+";")
		}
	}
	return out
}

// isApplyMechanism reports whether a statement is a config-apply mechanism
// the executor performs automatically, rather than a tuning recommendation.
func isApplyMechanism(stmt string) bool {
	u := strings.ToUpper(strings.TrimSpace(stmt))
	return strings.HasPrefix(u, "SELECT PG_RELOAD_CONF")
}

// rawStatementCount counts non-empty statements in the recommendation as
// the LLM emitted it, including apply mechanisms. Used to decide whether a
// single post-filter statement should keep its original verbatim shape.
func rawStatementCount(sql string) int {
	n := 0
	for _, s := range strings.Split(sql, ";") {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	return n
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
