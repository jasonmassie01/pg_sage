package migration

import (
	"errors"
	"regexp"
	"strings"
)

// ErrUntokenizable is SQL text that ends inside a literal, a quoted
// identifier, a dollar-quoted body or a block comment. The scanner
// redacts such a token to the end of the text, so it can hide statements:
// nothing in it is proven additive.
var ErrUntokenizable = errors.New("migration: SQL ends inside an unterminated " +
	"literal, quoted identifier, dollar-quoted body or block comment")

// UnprovenStatements returns, in order, every statement of sql that the
// additive allowlist does not prove additive, in the sanitized form
// DDLClassification.Statement holds. Additive means no table rewrite and
// no lock beyond the brief catalog update the allowlisted forms take:
//
//   - CREATE INDEX CONCURRENTLY, CREATE TABLE (not AS, PARTITION OF or
//     INHERITS), CREATE [OR REPLACE] VIEW, CREATE SCHEMA (without
//     embedded statements), COMMENT ON, CREATE SEQUENCE;
//   - ALTER TABLE whose every subcommand is ADD [COLUMN] name type, with
//     a built-in type and at most a constant or stable DEFAULT;
//   - SET [LOCAL|SESSION] lock_timeout or statement_timeout, BEGIN, START
//     TRANSACTION and COMMIT, which change no schema.
//
// Any statement that contains UNIQUE, REFERENCES, CHECK, EXCLUDE, PRIMARY
// KEY, LOGGED, ACCESS METHOD, NOT NULL, RENAME or TRIGGER is never proven
// additive, whatever its form. This is the floor the regex rules cannot
// lower (migration-04a); it is text-based, so it also catches a keyword
// in a view body or an index predicate.
func UnprovenStatements(sql string) ([]string, error) {
	clean, closed := scanDDL(sql)
	if !closed {
		return nil, ErrUntokenizable
	}
	var out []string
	for _, stmt := range splitStatements(clean) {
		stmt = collapseWhitespace(stmt)
		if !provenAdditive(pgWhitespace.ReplaceAllString(stmt, " ")) {
			out = append(out, stmt)
		}
	}
	return out, nil
}

var (
	// pgWhitespace is PostgreSQL's whitespace set. Go's \s lacks the
	// vertical tab, which PG 16+ reads as a space, so the checks below
	// see it as one (migration-04a review).
	pgWhitespace = regexp.MustCompile(`[\s\v]+`)

	// reLockingKeyword is the floor: these never appear in additive DDL.
	reLockingKeyword = regexp.MustCompile(`(?i)\b(?:UNIQUE|REFERENCES|CHECK|` +
		`EXCLUDE|PRIMARY\s+KEY|LOGGED|ACCESS\s+METHOD|NOT\s+NULL|RENAME|TRIGGER)\b`)

	// reNeutralStatement changes no schema: transaction control and the
	// timeouts a careful migration sets first.
	reNeutralStatement = regexp.MustCompile(`(?i)^(?:SET\s+(?:LOCAL\s+|SESSION\s+)?` +
		`(?:lock_timeout|statement_timeout)\s*(?:=|TO)\s*(?:'\*\*\*'|\d+|DEFAULT)` +
		`|BEGIN(?:\s+(?:WORK|TRANSACTION))?|START\s+TRANSACTION` +
		`|COMMIT(?:\s+(?:WORK|TRANSACTION))?)$`)

	reAdditiveCreate = regexp.MustCompile(
		`(?i)^(?:CREATE\s+(?:OR\s+REPLACE\s+)?VIEW|COMMENT\s+ON|CREATE\s+SEQUENCE)\s`)

	reCreateTable = regexp.MustCompile(`(?i)^CREATE\s+TABLE\s`)
	// reCreateTableLocks: PARTITION OF takes ACCESS EXCLUSIVE on the
	// parent, INHERITS locks the parent, and CREATE TABLE AS runs a query,
	// which PostgreSQL also accepts with no space before its parenthesis.
	reCreateTableLocks = regexp.MustCompile(`(?i)\bPARTITION\s+OF\b|\bINHERITS\b|` +
		`\bAS\b[\s(]*(?:SELECT|VALUES|TABLE|EXECUTE|WITH)\b`)

	reCreateSchema = regexp.MustCompile(`(?i)^CREATE\s+SCHEMA\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
		`(?:` + identRE + `(?:\s+AUTHORIZATION\s+` + identRE + `)?` +
		`|AUTHORIZATION\s+` + identRE + `)$`)

	reAlterTableRest = regexp.MustCompile(`(?i)^ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?` +
		`(?:ONLY\s+)?` + qualifiedRE + `\s+(.+)$`)
)

// provenAdditive reports whether one sanitized, collapsed statement is on
// the additive allowlist.
func provenAdditive(stmt string) bool {
	switch {
	case reLockingKeyword.MatchString(stmt):
		return false
	case reNeutralStatement.MatchString(stmt), reCreateIndexConcurrently.MatchString(stmt),
		reAdditiveCreate.MatchString(stmt), reCreateSchema.MatchString(stmt):
		return true
	case reCreateTable.MatchString(stmt):
		return !reCreateTableLocks.MatchString(stmt)
	}
	m := reAlterTableRest.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	for _, sub := range splitTopLevel(m[3]) {
		if !addColumnAdditive(sub) {
			return false
		}
	}
	return true
}

// splitTopLevel splits ALTER TABLE subcommands on the commas outside
// parentheses and quoted identifiers. Literals are already redacted.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			end, _ := skipIdentifier(s, i)
			i = end - 1
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}
