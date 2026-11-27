package cases

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// differsSuffix ends the next step of a head whose content the case no
// longer proposes, so it never reads as the current proposal's history.
const differsSuffix = "; the current proposal differs"

// recommendationNextStep says, in one line, what happened to the head and
// what pg_sage does next. differs is set when the case proposes content
// other than the head's.
func recommendationNextStep(r CaseRecommendation, differs bool, now time.Time) string {
	state := recommendation.State(r.State)
	if !state.Valid() {
		return fmt.Sprintf("Recommendation state unknown (%s); review it before acting", r.State)
	}
	if !differs {
		return stateStep(state, r, now)
	}
	if step, ok := earlierContentStep(state, r); ok {
		return step
	}
	return stateStep(state, r, now) + differsSuffix
}

// stateStep is the next step of a head in a known state.
func stateStep(state recommendation.State, r CaseRecommendation, now time.Time) string {
	switch state {
	case recommendation.StateReverted:
		return fmt.Sprintf("pg_sage applied this%s and reverted it: %s",
			onDate(r.AppliedAt), reasonOrNone(r.Reason))
	case recommendation.StateAbandoned:
		return "Stopped after " + countOf(r.AttemptCount, "failed attempt")
	case recommendation.StateFailed:
		return failedStep(r.NextAttemptAt, now)
	case recommendation.StateSuperseded:
		return "Superseded: the analyzer no longer proposes this change"
	case recommendation.StateVerified:
		return fmt.Sprintf("pg_sage applied this%s and verified it", onDate(r.AppliedAt))
	case recommendation.StateInconclusive:
		return fmt.Sprintf("pg_sage applied this%s; verification was inconclusive",
			onDate(r.AppliedAt))
	case recommendation.StateApplied:
		return fmt.Sprintf("pg_sage applied this%s; verification has not started",
			onDate(r.AppliedAt))
	case recommendation.StateVerifying:
		return fmt.Sprintf("pg_sage applied this%s; verification is in progress",
			onDate(r.AppliedAt))
	case recommendation.StateApplying:
		return "pg_sage is applying this now"
	case recommendation.StateApproved:
		return "Approved; waiting for the executor"
	default:
		return "Proposed; awaiting approval"
	}
}

// earlierContentStep describes a refused or failed head whose content the
// case no longer proposes.
func earlierContentStep(state recommendation.State, r CaseRecommendation) (string, bool) {
	switch state {
	case recommendation.StateReverted:
		return fmt.Sprintf("pg_sage reverted an earlier version%s: %s%s",
			onDate(&r.UpdatedAt), reasonOrNone(r.Reason), differsSuffix), true
	case recommendation.StateAbandoned:
		return "An earlier version stopped after " +
			countOf(r.AttemptCount, "failed attempt") + differsSuffix, true
	case recommendation.StateFailed:
		return "An earlier version failed" + differsSuffix, true
	default:
		return "", false
	}
}

func failedStep(next *time.Time, now time.Time) string {
	if !retryPending(next, now) {
		return "The last attempt failed; a retry is due"
	}
	at := next.UTC()
	if y, m, d := at.Date(); y == now.UTC().Year() && m == now.UTC().Month() &&
		d == now.UTC().Day() {
		return "Retrying after " + at.Format("15:04") + " UTC"
	}
	return "Retrying after " + at.Format("2006-01-02 15:04") + " UTC"
}

// onDate is " on 2006-01-02" (UTC), or "" for an unknown time.
func onDate(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return " on " + t.UTC().Format("2006-01-02")
}

func reasonOrNone(reason string) string {
	if reason == "" {
		return "no reason was recorded"
	}
	return reason
}

func countOf(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
