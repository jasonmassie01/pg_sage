package migration

import (
	"regexp"
	"strings"
)

// ruleByID returns the rule definition for a given rule ID.
// Panics if the rule does not exist (programming error).
func (rc *RegexClassifier) ruleByID(id string) ruleDefinition {
	for _, r := range rc.rules {
		if r.ID == id {
			return r
		}
	}
	panic("migration: unknown rule ID: " + id)
}

// newClassification builds a DDLClassification from a rule and SQL.
func newClassification(r ruleDefinition, sql string) DDLClassification {
	return DDLClassification{
		RuleID:          r.ID,
		Statement:       sql,
		LockLevel:       r.LockLevel,
		RequiresRewrite: r.RequiresRewrite,
		SafeAlternative: r.SafeAltTemplate,
		MinPGVersion:    r.MinPGVersion,
		Description:     r.Description,
	}
}

// fillTarget copies the (schema, name) groups of re's first match into
// c. Bare keywords (e.g. VACUUM FULL with no table) are ignored.
func fillTarget(re *regexp.Regexp, sql string, c *DDLClassification) {
	m := re.FindStringSubmatch(sql)
	if len(m) < 3 || !isColumnIdent(m[len(m)-1]) {
		return
	}
	if schema := m[len(m)-2]; schema != "" {
		c.SchemaName = unquoteIdent(schema)
	}
	c.TableName = unquoteIdent(m[len(m)-1])
}

// fillTableFromOnClause extracts schema.table from an "ON table" clause
// (e.g., CREATE INDEX ... ON myschema.mytable).
func fillTableFromOnClause(sql string, c *DDLClassification) {
	fillTarget(reIndexOnTable, sql, c)
}

// fillTableFromAlter extracts schema.table from ALTER TABLE statements.
func fillTableFromAlter(sql string, c *DDLClassification) {
	fillTarget(reAlterTable, sql, c)
}

// isDDLKeyword returns true if any statement in the (possibly
// multi-statement, commented) SQL starts with a DDL keyword that the
// migration advisor cares about.
func isDDLKeyword(sql string) bool {
	prefixes := []string{
		"ALTER ", "CREATE INDEX", "CREATE UNIQUE INDEX", "DROP ",
		"REINDEX", "VACUUM", "REFRESH ", "CLUSTER",
	}
	for _, stmt := range splitStatements(sanitizeDDL(sql)) {
		if hasAnyPrefix(strings.ToUpper(stmt), prefixes) {
			return true
		}
	}
	return false
}
