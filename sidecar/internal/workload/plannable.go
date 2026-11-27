package workload

import (
	"regexp"
	"strings"
)

// Plannable is the one check for what a planner may EXPLAIN (plan capture,
// the HypoPG what-if, the perf gate). After leading whitespace and
// comments, SELECT, VALUES, TABLE and WITH are queries, and INSERT,
// UPDATE, DELETE and MERGE are DML; a multi-statement string, utility,
// DDL, maintenance, EXPLAIN and procedures (CALL, DO) are not plannable.
var (
	leadRe    = regexp.MustCompile(`(?i)` + lead)
	queryVerb = regexp.MustCompile(`(?i)^(SELECT|VALUES|TABLE|WITH)` + end)
	dmlVerb   = regexp.MustCompile(`(?i)^(INSERT|UPDATE|DELETE|MERGE)` + end)
	withVerb  = regexp.MustCompile(`(?i)^WITH` + end)
	// dmlWord is a DML keyword anywhere: a WITH holding one may write.
	dmlWord = regexp.MustCompile(`(?i)(^|[^[:alnum:]_$])(INSERT|UPDATE|DELETE|MERGE)` + end)
)

// Plannable reports whether query is one statement a planner can EXPLAIN
// (ok) and whether it is read-only. A WITH is read-only only when no DML
// keyword appears in it at all, so a quoted one errs to not read-only.
func Plannable(query string) (ok, readOnly bool) {
	body := query[len(leadRe.FindString(query)):]
	if multiStatement(body) {
		return false, false
	}
	switch {
	case dmlVerb.MatchString(body):
		return true, false
	case withVerb.MatchString(body):
		return true, !dmlWord.MatchString(body)
	case queryVerb.MatchString(body):
		return true, true
	}
	return false, false
}

// multiStatement reports a semicolon before the end of body; trailing
// semicolons and whitespace end the one statement. A semicolon inside a
// literal also counts, which errs to not plannable.
func multiStatement(body string) bool {
	trimmed := strings.TrimRight(body, "; \t\r\n")
	return strings.Contains(trimmed, ";")
}
