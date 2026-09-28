package migration

import (
	"regexp"
)

// RegexClassifier implements SQLParser using regex/token matching.
// It is designed to be replaced by a pg_query_go-based classifier.
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
// statement. pgVersion is server_version_num (or a bare major version).
func (rc *RegexClassifier) Classify(sql string, pgVersion int) []DDLClassification {
	var results []DDLClassification
	lockTimeoutSet := false
	for _, stmt := range splitStatements(sanitizeDDL(sql)) {
		stmt = collapseWhitespace(stmt)
		if reLockTimeout.MatchString(stmt) {
			lockTimeoutSet = true
			continue
		}
		found := rc.classifyStatement(stmt, pgVersion)
		results = append(results, found...)
		if !lockTimeoutSet {
			results = append(results, rc.checkLockTimeout(stmt, found)...)
		}
	}
	return results
}

func (rc *RegexClassifier) classifyStatement(
	sql string, pgVersion int,
) []DDLClassification {
	var results []DDLClassification
	results = append(results, rc.matchIndexRules(sql)...)
	if reAlterTable.MatchString(sql) {
		results = append(results, rc.matchConstraintRules(sql)...)
		results = append(results, rc.matchAlterColumnRules(sql, pgVersion)...)
		results = append(results, rc.matchAddColumnRules(sql, pgVersion)...)
	}
	results = append(results, rc.matchDropRules(sql)...)
	results = append(results, rc.matchMaintenanceRules(sql, pgVersion)...)
	return results
}

var wsRegex = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return wsRegex.ReplaceAllString(s, " ")
}
