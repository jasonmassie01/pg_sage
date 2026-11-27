package migration

import (
	"regexp"
)

// RegexClassifier implements SQLParser using regex/token matching over
// sanitized, statement-split SQL.
type RegexClassifier struct {
	rules []ruleDefinition
}

// NewRegexClassifier creates a classifier with the full rule catalog.
func NewRegexClassifier() *RegexClassifier {
	return &RegexClassifier{rules: ruleCatalog()}
}

// Classify matches a SQL text against all rules and returns every
// matching classification. The text is sanitized first (comments
// stripped, literals redacted) and split into statements, so batches
// such as "SET lock_timeout = '5s'; ALTER TABLE ..." are classified per
// statement. The server version is accepted for the SQLParser contract
// but no regex rule depends on it: every rule holds on the supported
// floor (PostgreSQL 14) and later.
func (rc *RegexClassifier) Classify(sql string, version int) []DDLClassification {
	return classifyText(sql, version, func(stmt string, _ int) ([]DDLClassification, bool) {
		return rc.classifyStatement(stmt), true
	}).All()
}

func (rc *RegexClassifier) classifyStatement(sql string) []DDLClassification {
	var results []DDLClassification
	results = append(results, rc.matchIndexRules(sql)...)
	if reAlterTable.MatchString(sql) {
		results = append(results, rc.matchConstraintRules(sql)...)
		results = append(results, rc.matchAlterColumnRules(sql)...)
		results = append(results, rc.matchAddColumnRules(sql)...)
	}
	results = append(results, rc.matchDropRules(sql)...)
	results = append(results, rc.matchMaintenanceRules(sql)...)
	return results
}

var wsRegex = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return wsRegex.ReplaceAllString(s, " ")
}
