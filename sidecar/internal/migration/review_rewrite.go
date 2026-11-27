package migration

import (
	"regexp"
	"strings"
)

// guardLockTimeout is the guarded rewrite's lock bound.
const guardLockTimeout = "SET lock_timeout = '5s';"

// Statement shapes the guarded rewrite transforms. Each works on a
// sanitized, whitespace-collapsed statement.
var (
	reRewriteIndex   = regexp.MustCompile(`(?i)^(CREATE\s+(?:UNIQUE\s+)?INDEX)\s+`)
	reRewriteReindex = regexp.MustCompile(
		`(?i)^(REINDEX\s+(?:\([^)]*\)\s+)?(?:TABLE|INDEX|SCHEMA))\s+`)
	reRewriteRefresh   = regexp.MustCompile(`(?i)^(REFRESH\s+MATERIALIZED\s+VIEW)\s+`)
	reRewriteDropIndex = regexp.MustCompile(`(?i)^(DROP\s+INDEX)\s+`)
	reRewriteDetach    = regexp.MustCompile(`(?i)\sDETACH\s+PARTITION\s+\S+$`)
	reRewriteAddCons   = regexp.MustCompile(
		`(?i)^ALTER\s+TABLE\s+(.+?)\s+ADD\s+CONSTRAINT\s+("[^"]+"|[^\s"]+)\s+` +
			`(?:CHECK|FOREIGN\s+KEY)\b`)
	reRewriteNotNull = regexp.MustCompile(
		`(?i)^ALTER\s+TABLE\s+(.+?)\s+ALTER\s+(?:COLUMN\s+)?("[^"]+"|[^\s"]+)\s+` +
			`SET\s+NOT\s+NULL$`)
	reRewriteAddCol = regexp.MustCompile(
		`(?i)^ALTER\s+TABLE\s+(.+?)\s+ADD\s+(?:COLUMN\s+)?("[^"]+"|[^\s"]+)\s+(.+?)\s+` +
			`DEFAULT\s+(.+)$`)
	// reAddColExtra is a column definition the split rewrite does not
	// carry over safely: a constraint, generated column, collation, or
	// several subcommands.
	reAddColExtra = regexp.MustCompile(
		`(?i)\b(?:NOT\s+NULL|PRIMARY|UNIQUE|REFERENCES|CHECK|GENERATED|COLLATE|IDENTITY)\b|,`)
	reIdentUnsafe = regexp.MustCompile(`[^a-z0-9_]+`)
	reCascade     = regexp.MustCompile(`(?i)\sCASCADE$`)
)

// guardedRewrite returns the statement's deterministic safer form and the
// statement note: SET lock_timeout when the annotation fired, then the
// first rule rewrite that applies (CONCURRENTLY, NOT VALID + VALIDATE, the
// SET NOT NULL check, or a split ADD COLUMN). A statement holding a
// string literal is not rewritten: lint redacts literals, and a rewrite
// with a redacted literal would not run.
func guardedRewrite(sr StatementReview, risks []*DDLRisk, note string) (string, string) {
	if len(risks) == 0 && !sr.LockTimeoutMissing {
		return "", note
	}
	if strings.Contains(sr.Statement, redactedLiteral) {
		return "", joinNote(note, "No guarded rewrite: the statement holds a string "+
			"literal, which lint redacts; apply the safe alternative by hand.")
	}
	body := ""
	for _, r := range risks {
		if body = ruleRewrite(r.RuleID, sr.Statement); body != "" {
			break
		}
	}
	if body == "" && !sr.LockTimeoutMissing {
		return "", note
	}
	if body == "" {
		body = sr.Statement + ";"
	}
	if sr.LockTimeoutMissing {
		body = guardLockTimeout + "\n" + body
	}
	return body, note
}

func joinNote(note, more string) string {
	if note == "" {
		return more
	}
	return note + " " + more
}

// ruleRewrite is one rule's transformation of stmt, "" when the statement
// is not in a shape it rewrites.
func ruleRewrite(rule, stmt string) string {
	switch rule {
	case "ddl_index_not_concurrent":
		return concurrently(reRewriteIndex, stmt,
			"-- CONCURRENTLY cannot run inside a transaction block.")
	case "ddl_reindex_not_concurrent":
		return concurrently(reRewriteReindex, stmt, "")
	case "ddl_refresh_not_concurrent":
		return concurrently(reRewriteRefresh, stmt,
			"-- CONCURRENTLY needs a unique index on the materialized view.")
	case "ddl_drop_index_not_concurrent":
		// DROP INDEX CONCURRENTLY takes one index and no CASCADE.
		if hasTopLevelComma(stmt) || reCascade.MatchString(stmt) {
			return ""
		}
		return concurrently(reRewriteDropIndex, stmt,
			"-- CONCURRENTLY cannot run inside a transaction block.")
	case "ddl_detach_partition_not_concurrent":
		if reRewriteDetach.MatchString(stmt) {
			return stmt + " CONCURRENTLY;\n-- CONCURRENTLY cannot run inside a transaction block."
		}
	}
	return alterTableRewrite(rule, stmt)
}

// alterTableRewrite is the ALTER TABLE rules' transformation. A statement
// with several subcommands is not rewritten: the rewrite would make only
// one of them safe and present the rest, which still scan, as guarded.
func alterTableRewrite(rule, stmt string) string {
	if hasTopLevelComma(stmt) {
		return ""
	}
	switch rule {
	case "ddl_constraint_not_valid", "ddl_fk_not_valid":
		return notValidRewrite(stmt)
	case "ddl_set_not_null":
		return notNullRewrite(stmt)
	case "ddl_add_column_volatile_default":
		return splitAddColumn(stmt)
	}
	return ""
}

// hasTopLevelComma reports a ',' outside parentheses and double-quoted
// identifiers: one statement listing several subcommands or objects. The
// text is sanitized, so string literals are already '***'.
func hasTopLevelComma(s string) bool {
	depth, quoted := 0, false
	for _, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth = max(depth-1, 0)
		case c == ',' && depth == 0:
			return true
		}
	}
	return false
}

func concurrently(re *regexp.Regexp, stmt, comment string) string {
	if !re.MatchString(stmt) {
		return ""
	}
	out := re.ReplaceAllString(stmt, "$1 CONCURRENTLY ") + ";"
	if comment != "" {
		out += "\n" + comment
	}
	return out
}

func notValidRewrite(stmt string) string {
	m := reRewriteAddCons.FindStringSubmatch(stmt)
	if m == nil || strings.Contains(strings.ToUpper(stmt), "NOT VALID") {
		return ""
	}
	return stmt + " NOT VALID;\nALTER TABLE " + m[1] + " VALIDATE CONSTRAINT " + m[2] + ";"
}

// notNullRewrite proves the column non-null with a CHECK added NOT VALID
// and validated under SHARE UPDATE EXCLUSIVE; SET NOT NULL then skips its
// scan (PostgreSQL 12+), and the helper constraint is dropped.
func notNullRewrite(stmt string) string {
	m := reRewriteNotNull.FindStringSubmatch(stmt)
	if m == nil {
		return ""
	}
	table, col := m[1], m[2]
	name := reIdentUnsafe.ReplaceAllString(strings.ToLower(strings.Trim(col, `"`)), "_") +
		"_not_null"
	return "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " CHECK (" + col +
		" IS NOT NULL) NOT VALID;\n" +
		"ALTER TABLE " + table + " VALIDATE CONSTRAINT " + name + ";\n" +
		"ALTER TABLE " + table + " ALTER COLUMN " + col + " SET NOT NULL;\n" +
		"ALTER TABLE " + table + " DROP CONSTRAINT " + name + ";"
}

// splitAddColumn adds the column without the volatile default (no
// rewrite), sets the default for new rows, and leaves the backfill of
// existing rows to batches.
func splitAddColumn(stmt string) string {
	m := reRewriteAddCol.FindStringSubmatch(stmt)
	if m == nil || reAddColExtra.MatchString(m[3]) || reAddColExtra.MatchString(m[4]) {
		return ""
	}
	table, col, typ, def := m[1], m[2], m[3], m[4]
	return "ALTER TABLE " + table + " ADD COLUMN " + col + " " + typ + ";\n" +
		"ALTER TABLE " + table + " ALTER COLUMN " + col + " SET DEFAULT " + def + ";\n" +
		"-- Backfill existing rows in batches, then add any constraint:\n" +
		"-- UPDATE " + table + " SET " + col + " = " + def +
		" WHERE " + col + " IS NULL AND <key range>;"
}
