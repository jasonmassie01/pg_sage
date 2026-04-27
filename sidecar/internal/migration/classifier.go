package migration

import (
	"regexp"
	"strings"
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

// Classify matches a SQL statement against all rules and returns
// every matching classification. pgVersion is the major PG version
// (e.g. 14). Pass 0 to skip version-gated checks.
func (rc *RegexClassifier) Classify(sql string, pgVersion int) []DDLClassification {
	sql = normalizeSQL(sql)
	var results []DDLClassification

	results = append(results, rc.matchIndexRules(sql)...)
	results = append(results, rc.matchConstraintRules(sql)...)
	results = append(results, rc.matchAlterColumnRules(sql, pgVersion)...)
	results = append(results, rc.matchAddColumnRules(sql, pgVersion)...)
	results = append(results, rc.matchDropRules(sql)...)
	results = append(results, rc.matchMaintenanceRules(sql, pgVersion)...)
	results = append(results, rc.checkLockTimeout(sql, results)...)

	return results
}

// normalizeSQL collapses whitespace for regex matching while
// preserving the original statement text in classifications.
func normalizeSQL(sql string) string {
	s := strings.TrimSpace(sql)
	return collapseWhitespace(s)
}

var wsRegex = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string {
	return wsRegex.ReplaceAllString(s, " ")
}
