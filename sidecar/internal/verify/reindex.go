package verify

import "fmt"

// Reindex verification (dogfood round 2). REINDEX is hygiene: it is
// proposed for bloated indexes, so it is judged by the bytes it reclaimed
// and by the rebuilt index being valid, with no regression of the queries
// on its table. It cannot be rolled back: a regression is recorded for
// the trust ledger, never undone.

// IndexShrinkPct is how much smaller the rebuilt index must be for the
// REINDEX to count as an improvement rather than a neutral rebuild.
const IndexShrinkPct = 10

// SizeJudgement is the REINDEX target's verdict from its size and
// validity.
type SizeJudgement struct {
	Verdict   string
	Reason    string
	ChangePct *float64
	Before    float64
	After     float64
}

// JudgeIndexSize compares the target's bytes before and after: an invalid
// index after the rebuild is a regression; at least IndexShrinkPct smaller
// is improved; anything else is a neutral rebuild (no bloat to reclaim).
func JudgeIndexSize(before, after int64, valid bool) SizeJudgement {
	j := SizeJudgement{Before: float64(before), After: float64(after)}
	if before <= 0 || after < 0 {
		j.Verdict, j.Reason = OutcomeUnverifiable, "index size could not be measured"
		return j
	}
	change := float64(after-before) * 100 / float64(before)
	j.ChangePct = &change
	switch {
	case !valid:
		j.Verdict, j.Reason = OutcomeRegressed, "an index is invalid after the REINDEX"
	case after*100 <= before*(100-IndexShrinkPct):
		j.Verdict = OutcomeImproved
		j.Reason = fmt.Sprintf("index reclaimed %.1f%% (%d -> %d bytes)", -change,
			before, after)
	default:
		j.Verdict = OutcomeNeutral
		j.Reason = fmt.Sprintf("index size held (%d -> %d bytes): no bloat reclaimed",
			before, after)
	}
	return j
}

// DecideReindex combines the table's queries with the size judgement;
// accruing reports a verdict more time can still change. A regression of
// either is a regression; with queries on the table the size counts once
// they are measured not to have regressed; a table no statement reads is
// judged on its size alone.
func DecideReindex(time *Comparison, size SizeJudgement) (string, string, bool) {
	switch {
	case time != nil && time.Verdict == OutcomeRegressed:
		return OutcomeRegressed, "queries on the table regressed: " + time.Reason, false
	case size.Verdict == OutcomeRegressed || size.Verdict == OutcomeUnverifiable:
		return size.Verdict, size.Reason, false
	case time == nil:
		return size.Verdict, size.Reason + "; no statement reads the table", false
	case time.Verdict == OutcomeInsufficient:
		return OutcomeInsufficient, size.Reason + "; reads not yet measurable: " +
			time.Reason, true
	}
	return size.Verdict, size.Reason + "; reads " + time.Verdict + ": " + time.Reason, false
}
