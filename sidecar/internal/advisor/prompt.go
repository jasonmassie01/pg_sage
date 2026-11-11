package advisor

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const maxAdvisorPromptChars = 16384

// configCategories are sub-advisor categories whose value is the SQL
// change itself; a recommendation without SQL is dropped, unless its kind
// is one of infoKinds.
var configCategories = map[string]bool{
	"vacuum_tuning": true, "wal_tuning": true,
	"memory_tuning": true, "connection_tuning": true,
}

// parseLLMFindings parses the LLM JSON response into findings. Model
// output is data: each finding's Detail is built by advisorDetail, so
// the model can never set a key pg_sage's own producers set.
func parseLLMFindings(
	raw string,
	category string,
	logFn func(string, string, ...any),
) []analyzer.Finding {
	var recs []map[string]any
	if err := llm.ParseJSON(raw, llm.JSONArray, &recs); err != nil {
		if logFn != nil {
			logFn("WARN", "advisor: %s: parse error: %v", category, err)
		}
		return nil
	}

	var findings []analyzer.Finding
	for _, rec := range recs {
		findings = append(findings, recFindings(category, rec, logFn)...)
	}
	return applyConfigAllowlist(findings)
}

// recFindings turns one model recommendation into findings, one per
// statement it carries.
func recFindings(category string, rec map[string]any,
	logFn func(string, string, ...any)) []analyzer.Finding {
	detail, dropped := advisorDetail(category, rec)
	if len(dropped) > 0 && logFn != nil {
		// %q: the key names are model output, so quote them in the log.
		logFn("WARN", "advisor: %s: dropped %d reserved detail key(s) from model "+
			"output: %q", category, len(dropped), dropped)
	}
	objID := recObjectID(rec)
	severity, _ := rec["severity"].(string)
	if severity == "" {
		severity = "info"
	}
	rationale, _ := rec["rationale"].(string)
	recSQL, _ := rec["recommended_sql"].(string)
	if kind := infoKind(category, rec, rationale, recSQL); kind != "" {
		return []analyzer.Finding{infoFinding(category, objID, kind, detail, rationale)}
	}

	stmts, multi := recStatements(category, recSQL)
	var findings []analyzer.Finding
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
			if set, ok := pgconf.ParseAlterSystem(stmt); ok && !set.Reset {
				oid = objID + ":" + set.Name
				title = fmt.Sprintf("%s: set %s", category, set.Name)
			}
		}
		findings = append(findings, analyzer.Finding{
			Category: category, Severity: severity, ObjectType: "configuration",
			ObjectIdentifier: oid, Title: title, Detail: detail,
			Recommendation: rationale, RecommendedSQL: stmt,
			ActionRisk: deriveActionRisk(stmt),
		})
	}
	return findings
}

// infoKinds are the kinds of a config sub-advisor's row that carry advice
// with no setting change (advisors-07): pooler advice and observations.
var infoKinds = map[string]bool{"pooler": true, "observation": true}

// infoKind is the row's kind when it is kept as an info finding without
// SQL: a config category, no statement at all, a rationale, and an info
// kind. Any other SQL-less config row is a "no change" row and is dropped.
func infoKind(category string, rec map[string]any, rationale, recSQL string) string {
	kind, _ := rec["kind"].(string)
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !configCategories[category] || !infoKinds[kind] ||
		strings.TrimSpace(rationale) == "" || rawStatementCount(recSQL) > 0 {
		return ""
	}
	return kind
}

// recObjectID is the object a recommendation is about: its identifier, its
// table, else the instance.
func recObjectID(rec map[string]any) string {
	if id, _ := rec["object_identifier"].(string); id != "" {
		return id
	}
	if table, _ := rec["table"].(string); table != "" {
		return table
	}
	return "instance"
}

// infoFinding is a config row kept as advice: info, no SQL, rollback or
// risk, one open row per kind.
func infoFinding(category, objID, kind string, detail map[string]any,
	rationale string) analyzer.Finding {
	return analyzer.Finding{Category: category, Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: objID + ":" + kind,
		Title: fmt.Sprintf("%s: %s advice", category, kind), Detail: detail,
		Recommendation: rationale}
}

// recStatements returns the statements a recommendation yields and
// whether it was split from several. A single recommendation may carry
// several config statements (e.g. WAL tuning sets checkpoint_timeout,
// max_wal_size, min_wal_size). Split them so each is an atomic,
// individually validated and reloadable action — the executor rejects
// multi-statement SQL. A single statement keeps its original shape.
func recStatements(category, recSQL string) ([]string, bool) {
	stmts := splitSQLStatements(recSQL)
	multi := len(stmts) > 1
	switch {
	case len(stmts) == 0 && rawStatementCount(recSQL) > 0:
		// recSQL was non-empty but only an apply mechanism (e.g. a bare
		// reload) — nothing actionable to record.
		return nil, false
	case len(stmts) == 0 && configCategories[category]:
		// "No changes needed" rows are not findings; persisted, they
		// only blocked the sub-advisor (G3-B18).
		return nil, false
	case len(stmts) == 0:
		// Advisory categories keep one finding carrying no SQL.
		return []string{recSQL}, false
	case !multi && rawStatementCount(recSQL) == 1:
		// Single statement to begin with — keep its verbatim shape.
		return []string{recSQL}, false
	}
	return stmts, multi
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
