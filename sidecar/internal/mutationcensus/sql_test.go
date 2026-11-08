package mutationcensus

import (
	"regexp"
	"strings"
)

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// statementBreak marks where a new statement can start: a semicolon, a
	// dollar quote, a PL/pgSQL block keyword or a CTE body.
	statementBreak = regexp.MustCompile(`(?i);|\$[A-Za-z_]*\$|\b(?:BEGIN|THEN|ELSE|LOOP)\b|` +
		`\bAS\s+(?:NOT\s+)?(?:MATERIALIZED\s+)?\(`)
	writeVerb = regexp.MustCompile(`(?is)^(INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE|` +
		`MERGE\s+INTO|CREATE|ALTER|DROP|COMMENT\s+ON|GRANT|REVOKE|VACUUM|ANALYZE|` +
		`REINDEX|CLUSTER|REFRESH\s+MATERIALIZED)\b`)
	updateSet = regexp.MustCompile(`(?is)^UPDATE\b.*\bSET\b`)
	// stateFunc names the functions that change state other sessions see:
	// a hypothetical index, a backend signal, a statistics reset, a reload.
	stateFunc = regexp.MustCompile(`(?i)\b(?:hypopg_create_index|pg_cancel_backend|` +
		`pg_terminate_backend|pg_stat_statements_reset|pg_stat_reset\w*|pg_reload_conf)"?\s*\(`)
	headStop = regexp.MustCompile(`(?i)\b(?:SELECT|VALUES|WHERE|SET|USING|RETURNING|AS|IS|` +
		`TO|FROM|DEFAULT|RETURNS|WITH)\b`)
	grantOn      = regexp.MustCompile(`(?i)\bON\b`)
	qualified    = regexp.MustCompile(`("?)([A-Za-z_][A-Za-z0-9_$]*)"?\s*\.`)
	sageSchemaOp = regexp.MustCompile(`(?i)^(?:(?:CREATE|ALTER|DROP)\s+SCHEMA|COMMENT\s+ON\s+` +
		`SCHEMA|(?:GRANT|REVOKE)\b.*\bON\s+SCHEMA)\s+(?:IF\s+(?:NOT\s+)?EXISTS\s+)?` +
		`(?:"sage"|sage)(?:\s|$|,|;)`)
)

// writeStatements returns each statement of a SQL text that writes to an
// object outside the sage schema.
func writeStatements(text string) []string {
	text = sqlBlockComment.ReplaceAllString(text, " ")
	text = sqlLineComment.ReplaceAllString(text, " ")
	var writes []string
	for _, piece := range statementBreak.Split(text, -1) {
		stmt := strings.TrimSpace(piece)
		if stmt == "" || !isWrite(stmt) || targetsSage(stmt) {
			continue
		}
		writes = append(writes, stmt)
	}
	return writes
}

func isWrite(stmt string) bool {
	if stateFunc.MatchString(stmt) {
		return true
	}
	m := writeVerb.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	if strings.EqualFold(m[1], "UPDATE") {
		return updateSet.MatchString(stmt)
	}
	return true
}

// targetsSage reports whether a write statement's target is in the sage
// schema: the first schema-qualified name of the statement head (the text
// after the verb, up to its first clause keyword) is sage.*.
func targetsSage(stmt string) bool {
	if stateFunc.MatchString(stmt) {
		return false
	}
	if sageSchemaOp.MatchString(stmt + " ") {
		return true
	}
	verb := writeVerb.FindString(stmt)
	head := stmt[len(verb):]
	upper := strings.ToUpper(verb)
	if upper == "GRANT" || upper == "REVOKE" {
		loc := grantOn.FindStringIndex(head)
		if loc == nil {
			return false
		}
		head = head[loc[1]:]
	}
	if loc := headStop.FindStringIndex(head); loc != nil {
		head = head[:loc[0]]
	}
	m := qualified.FindStringSubmatch(head)
	if m == nil {
		return false
	}
	if m[1] == `"` {
		return m[2] == "sage"
	}
	return strings.EqualFold(m[2], "sage")
}
