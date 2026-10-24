package main

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The CI gate of a review: whether the job passes under --fail-on and the
// commit status GitHub mode sets. A DBA verdict decides once it exists: a
// concurrence (with or without edits) or a too-strict override passes; a
// too-lenient override fails. A review that is still running or failed
// does not pass unless --fail-on never (fail closed: an undecided review
// is not a safe one).

// failOnRank orders --fail-on like the verdicts; never fails on nothing.
var failOnRank = map[string]int{"review": 1, "risky": 2, "never": 99}

var verdictRank = map[string]int{changereview.VerdictSafe: 0,
	changereview.VerdictReview: 1, changereview.VerdictRisky: 2}

// reviewGate is a GitHub commit status (state, description) and whether
// the CLI exits 0.
type reviewGate struct {
	state, description string
	pass               bool
}

const maxStatusDescription = 140

func gateOf(r changereview.Review, failOn string) reviewGate {
	never := failOn == "never"
	switch r.State {
	case changereview.StateRunning:
		return reviewGate{state: "pending", description: "pg_sage is reviewing the change",
			pass: never}
	case changereview.StateFailed:
		return reviewGate{state: "error", description: trimmed("pg_sage review failed: "+
			r.FailureReason, maxStatusDescription), pass: never}
	}
	if g, ok := dbaGate(r, never); ok {
		return g
	}
	counts := fmt.Sprintf("%d critical, %d warning", r.Summary.Critical, r.Summary.Warning)
	if verdictRank[r.Verdict] >= failOnRank[failOn] {
		return reviewGate{state: "failure", description: r.Verdict + ": " + counts +
			"; a DBA verdict is needed", pass: false}
	}
	desc := r.Verdict + ": " + counts
	switch r.Verdict {
	case changereview.VerdictSafe:
		desc = "safe: no blocking findings"
	case changereview.VerdictReview:
		desc += "; a DBA review is requested"
	}
	return reviewGate{state: "success", description: desc, pass: true}
}

// dbaGate is the gate of a review a DBA decided.
func dbaGate(r changereview.Review, never bool) (reviewGate, bool) {
	by := ""
	if r.DBAActor != "" {
		by = " (" + r.DBAActor + ")"
	}
	switch {
	case r.DBAVerdict == changereview.DBAConcur:
		return reviewGate{state: "success", description: "DBA concurred" + by, pass: true}, true
	case r.DBAVerdict == changereview.DBAConcurWithEdits:
		return reviewGate{state: "success", description: "DBA concurred with edits" + by,
			pass: true}, true
	case r.DBAVerdict == changereview.DBAOverride &&
		r.DBADirection == changereview.DirectionTooLenient:
		return reviewGate{state: "failure", description: "DBA override: pg_sage was too " +
			"lenient" + by, pass: never}, true
	case r.DBAVerdict == changereview.DBAOverride:
		return reviewGate{state: "success", description: "DBA override: pg_sage was too " +
			"strict" + by, pass: true}, true
	}
	return reviewGate{}, false
}
