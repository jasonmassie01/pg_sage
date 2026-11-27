// Package workload is the one rule for which statements are application
// workload: the input of every advice path (slow-query, plan-regression
// and top-query findings, the LLM advisors, the tuner, plan capture,
// verification targets and the briefing). pg_stat_statements also tracks
// pg_sage's own statements (tagged /* pg_sage */, see selfmonitor) and
// diagnostic tooling an operator or a dogfood session ran by hand: EXPLAIN
// [ANALYZE], maintenance (VACUUM, ANALYZE, CREATE INDEX, REINDEX, CLUSTER,
// CHECKPOINT), statistics resets and backup COPY ... TO. None of those is
// workload to tune (lifeos 2026-10-04: the briefing told the operator to
// "investigate" an untagged EXPLAIN ANALYZE of an old pg_sage probe).
// Nor are utility statements (transaction control, SET, DISCARD, LOCK,
// LISTEN/NOTIFY: tracked with track_utility = on) or DDL and privilege
// changes: no index or hint can fix a slow COMMIT or ALTER TABLE.
// Procedures (CALL, DO) stay workload, but no planner can EXPLAIN them;
// Plannable is the one check for what a planner may EXPLAIN.
//
// The rule only filters advice. The collector keeps every statement, so
// raw query views (snapshots) still show them, pg_sage's self-cost reads
// its own tagged statements, and rules about capacity, I/O and incidents
// (RCA, SRE probes) still count them: a backup or an index build can be
// the cause of an incident even though it is never the subject of advice.
//
// Classify (Go) and DiagnosticSQL/AdviceSQL (PostgreSQL) are the same
// pattern; a parity test runs both on real Postgres.
package workload

import (
	"regexp"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// Reason says why a statement is not workload ("" when it is).
type Reason string

// Statement classes.
const (
	Workload    Reason = ""
	Self        Reason = "pg_sage"
	Explain     Reason = "explain"
	Maintenance Reason = "maintenance"
	StatsReset  Reason = "stats_reset"
	Backup      Reason = "backup"
	Utility     Reason = "utility"
	DDL         Reason = "ddl"
	// Procedural (CALL, DO) is workload for findings, never plannable.
	Procedural Reason = "procedural"
)

// Pattern parts, written in the common subset of RE2 (Go) and PostgreSQL
// advanced regular expressions: no \b (a backspace in PostgreSQL), no
// lookaround, POSIX classes only inside brackets. Matched
// case-insensitively.
const (
	// lead skips whitespace and comments before the first keyword. A --
	// comment ends at LF or CR, as in the PostgreSQL lexer.
	lead = `^(\s|/\*([^*]|\*+[^*/])*\*+/|--[^\n\r]*([\n\r]|$))*`
	// end is the end of a keyword: not followed by an identifier character.
	end         = `([^[:alnum:]_$]|$)`
	explainPart = `EXPLAIN` + end
	maintPart   = `(VACUUM|ANALY[SZ]E|CLUSTER|REINDEX|CHECKPOINT|` +
		`CREATE\s+(UNIQUE\s+)?INDEX)` + end
	resetPart = `SELECT\s+(PG_CATALOG\s*\.\s*)?PG_STAT_(STATEMENTS_)?RESET` +
		`[[:alnum:]_]*\s*\(`
	ident = `("([^"]|"")+"|[[:alpha:]_][[:alnum:]_$]*)`
	// backupPart is COPY of a query (always TO) or of a relation, with an
	// optional column list, TO a file or client.
	backupPart = `COPY\s*\(|COPY\s+(BINARY\s+)?` + ident + `(\s*\.\s*` + ident +
		`)*(\s*\([^)]*\))?\s+TO` + end
	// utilityPart is transaction control and session utility statements.
	utilityPart = `(BEGIN|START|COMMIT|END|ROLLBACK|ABORT|SAVEPOINT|RELEASE|` +
		`PREPARE\s+TRANSACTION|SET|RESET|SHOW|DISCARD|DEALLOCATE|LISTEN|NOTIFY|` +
		`UNLISTEN|LOCK)` + end
	// ddlPart is schema and privilege changes; CREATE INDEX matches
	// maintPart first.
	ddlPart           = `(CREATE|ALTER|DROP|TRUNCATE|COMMENT|GRANT|REVOKE)` + end
	procPart          = `(CALL|DO)` + end
	diagnosticPattern = lead + `(` + explainPart + `|` + maintPart + `|` + resetPart +
		`|` + backupPart + `|` + utilityPart + `|` + ddlPart + `)`
)

// procedural matches CALL and DO, which are workload (not in
// diagnosticPattern) but not plannable.
var procedural = regexp.MustCompile(`(?i)` + lead + procPart)

// diagnosticKinds are the alternatives of diagnosticPattern, one per
// reason; a statement is diagnostic when one of them matches.
var diagnosticKinds = []struct {
	re     *regexp.Regexp
	reason Reason
}{
	{regexp.MustCompile(`(?i)` + lead + explainPart), Explain},
	{regexp.MustCompile(`(?i)` + lead + maintPart), Maintenance},
	{regexp.MustCompile(`(?i)` + lead + resetPart), StatsReset},
	{regexp.MustCompile(`(?i)` + lead + `(` + backupPart + `)`), Backup},
	{regexp.MustCompile(`(?i)` + lead + utilityPart), Utility},
	{regexp.MustCompile(`(?i)` + lead + ddlPart), DDL},
}

// diagnosticReason is which diagnostic tooling query is (Workload: none).
func diagnosticReason(query string) Reason {
	for _, k := range diagnosticKinds {
		if k.re.MatchString(query) {
			return k.reason
		}
	}
	return Workload
}

// Classify names what kind of statement query is: workload, a procedure,
// pg_sage's own, or which diagnostic, utility or DDL statement.
func Classify(query string) Reason {
	if selfmonitor.IsQueryText(query) {
		return Self
	}
	if r := diagnosticReason(query); r != Workload {
		return r
	}
	if procedural.MatchString(query) {
		return Procedural
	}
	return Workload
}

// Excluded reports a statement advice must leave out (procedures stay).
func Excluded(query string) bool {
	r := Classify(query)
	return r != Workload && r != Procedural
}

// IsDiagnostic reports diagnostic tooling, utility or DDL, whoever ran it
// (the pattern alone; DiagnosticSQL is its SQL twin).
func IsDiagnostic(query string) bool { return diagnosticReason(query) != Workload }

// Queries returns the workload statements of qs, in order, in a new slice;
// qs is left as it is (raw views and capacity rules read every entry).
func Queries(qs []collector.QueryStats) []collector.QueryStats {
	out := make([]collector.QueryStats, 0, len(qs))
	for _, q := range qs {
		if !Excluded(q.Query) {
			out = append(out, q)
		}
	}
	return out
}

func textExpr(column string) string {
	if column == "" {
		column = "query"
	}
	return "COALESCE(" + column + ", '')"
}

// DiagnosticSQL is a predicate, true when the text expression column is
// diagnostic tooling, utility or DDL (IsDiagnostic in SQL).
func DiagnosticSQL(column string) string {
	return textExpr(column) + " ~* '" + diagnosticPattern + "'"
}

// AdviceSQL is a predicate, true when the text expression column (default
// query) is workload: not pg_sage's own and not diagnostic tooling,
// utility or DDL. It is plain SQL (no % to escape) and NULL text counts
// as workload.
func AdviceSQL(column string) string {
	if column == "" {
		column = "query"
	}
	return selfmonitor.StatementExclusionSQL(column) + " AND NOT (" +
		DiagnosticSQL(column) + ")"
}

// detailQueryKeys are the finding detail keys that carry a statement.
var detailQueryKeys = []string{"query", "query_text", "normalized_query",
	"sample_query", "statement"}

// FindingExcluded reports a finding about a statement advice leaves out:
// one of its detail statement keys holds an excluded statement.
func FindingExcluded(detail map[string]any) bool {
	for _, key := range detailQueryKeys {
		var text string
		switch v := detail[key].(type) {
		case string:
			text = v
		case []byte:
			text = string(v)
		default:
			continue
		}
		if Excluded(text) {
			return true
		}
	}
	return false
}

// FindingAdviceSQL is a predicate over sage.findings, true when no detail
// statement key of the jsonb column holds an excluded statement.
func FindingAdviceSQL(detailColumn string) string {
	out := ""
	for i, key := range detailQueryKeys {
		if i > 0 {
			out += " AND "
		}
		out += "(" + AdviceSQL(detailColumn+"->>'"+key+"'") + ")"
	}
	return out
}
