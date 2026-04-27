package migration

import "strings"

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

// fillTableFromOnClause extracts schema.table from an "ON table" clause
// (e.g., CREATE INDEX ... ON myschema.mytable).
func fillTableFromOnClause(sql string, c *DDLClassification) {
	m := reIndexOnTable.FindStringSubmatch(sql)
	if m == nil {
		return
	}
	if m[2] != "" {
		c.SchemaName = m[2]
	}
	c.TableName = m[3]
}

// fillTableFromAlter extracts schema.table from ALTER TABLE statements.
func fillTableFromAlter(sql string, c *DDLClassification) {
	m := reAlterTable.FindStringSubmatch(sql)
	if m == nil {
		return
	}
	if m[2] != "" {
		c.SchemaName = m[2]
	}
	c.TableName = m[3]
}

// isDDLKeyword returns true if the trimmed, uppercased SQL starts with
// a DDL keyword that the migration advisor cares about.
func isDDLKeyword(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	prefixes := []string{
		"ALTER ", "CREATE INDEX", "DROP ",
		"REINDEX", "VACUUM", "REFRESH ", "CLUSTER",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}
