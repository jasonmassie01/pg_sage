package migration

import (
	"fmt"
	"strings"
)

// Verdict bars on the risk score.
const (
	reviewRiskyScore = 0.7
	scanRewriteBar   = 0.6 // rewriteWeight of a statement that scans or rewrites
)

var verdictRank = map[string]int{VerdictSafe: 0, VerdictReview: 1, VerdictRisky: 2}

// worstVerdict returns the stricter of two verdicts.
func worstVerdict(a, b string) string {
	if verdictRank[b] > verdictRank[a] {
		return b
	}
	return a
}

// hazardVerdict grades one matched rule. A matched rule is never safe: it
// locks or rewrites a table. It is risky at a score of 0.7, or when the
// table holds at least the row threshold and the statement keeps writes
// blocked for as long as the table is big: a scan or rewrite under SHARE
// or stronger, or a metadata-only ACCESS EXCLUSIVE with no lock_timeout
// (it queues every reader behind the slowest transaction). Below the
// threshold, or with the size unknown (static mode, never analyzed), the
// score alone decides.
func hazardVerdict(r *DDLRisk, rowThreshold int64, lockTimeoutSet bool) string {
	if r.RiskScore >= reviewRiskyScore {
		return VerdictRisky
	}
	if r.StatsKnown && r.EstimatedRows >= rowThreshold && blocksForSize(r, lockTimeoutSet) {
		return VerdictRisky
	}
	return VerdictReview
}

func blocksForSize(r *DDLRisk, lockTimeoutSet bool) bool {
	if lockLevelWeight(r.LockLevel) < lockLevelWeight("SHARE") {
		return false
	}
	if rewriteWeight(r) >= scanRewriteBar {
		return true
	}
	return r.LockLevel == "ACCESS EXCLUSIVE" && !lockTimeoutSet
}

// finishReport sets the report's score and verdict and the notes on what
// is not proven: an untokenizable text, unrecognized statements and a
// build without the SQL parser.
func finishReport(rep *Report, cls Classification) {
	rep.Verdict = VerdictSafe
	for _, st := range rep.Statements {
		rep.Verdict = worstVerdict(rep.Verdict, st.Verdict)
		for _, h := range st.Hazards {
			rep.MaxRiskScore = max(rep.MaxRiskScore, h.RiskScore)
		}
	}
	if cls.Untokenizable {
		rep.Verdict = worstVerdict(rep.Verdict, VerdictReview)
		rep.Notes = append(rep.Notes, "The SQL could not be tokenized: it ends inside a "+
			"literal, quoted identifier, dollar-quoted body or comment, which can hide "+
			"statements. Review it.")
	}
	if n := cls.Unrecognized(); n > 0 {
		rep.Notes = append(rep.Notes, unrecognizedNote(n, cls.Degraded))
	}
	if rep.Static {
		rep.Notes = append(rep.Notes, "Static review: no database was read, so table "+
			"sizes are unknown and only intrinsic hazards are scored.")
	}
}

// unrecognizedNote counts the statements no rule or safe form recognized.
func unrecognizedNote(n int, degraded bool) string {
	known := "a known safe form"
	if degraded {
		known = "on the additive allowlist (this build has no SQL parser)"
	}
	if n == 1 {
		return "1 statement is not " + known + " and matches no DDL safety rule: " +
			"unrecognized DDL is not proven safe. Review it."
	}
	return fmt.Sprintf("%d statements are not %s and match no DDL safety rule: "+
		"unrecognized DDL is not proven safe. Review them.", n, known)
}

// ExitCode is the CI contract: 0 safe, 1 review, 2 risky. A tool error is
// 3, which the caller returns itself, so a broken gate never reads as a
// review.
func (r Report) ExitCode() int {
	switch r.Verdict {
	case VerdictSafe:
		return 0
	case VerdictReview:
		return 1
	case VerdictRisky:
		return 2
	}
	return 3
}

// statementLines returns each statement's 1-based start line in sql, or
// zeros when the scan does not find exactly want statements. It walks the
// same tokens as scanDDL, so a ';' in a literal, comment or quoted
// identifier does not split.
func statementLines(sql string, want int) []int {
	lines := make([]int, want)
	var found []int
	start := -1
	for i := 0; i < len(sql); {
		next, token, _ := scanToken(sql, i)
		switch {
		case token == ";" && sql[i] == ';':
			found = appendLine(found, sql, start)
			start = -1
		case start < 0 && strings.TrimSpace(token) != "":
			start = i
		}
		i = next
	}
	found = appendLine(found, sql, start)
	if len(found) == want {
		copy(lines, found)
	}
	return lines
}

func appendLine(found []int, sql string, start int) []int {
	if start < 0 {
		return found
	}
	return append(found, strings.Count(sql[:start], "\n")+1)
}
