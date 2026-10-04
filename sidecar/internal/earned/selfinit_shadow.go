package earned

import "fmt"

// Shadow evidence in the promotion bar (roadmap 1.4). A scored shadow
// decision the ledger does not already hold as a real outcome (a change
// applied outside pg_sage and verified, or a HypoPG what-if of an index
// create) counts for its class like a real outcome of the family:
// correct is a success, neutral is decided but uncredited, incorrect
// resets the streak (a demerit for promotion; it never demotes). L2 (one
// click per action) may be earned from shadow evidence alone; L3
// (unattended) needs MinRealSuccessesL3 real verified successes, and
// shadow successes fill at most the rest of its bar.

// MinRealSuccessesL3 is the minimum real verified successes (since the
// last demerit) a class needs for L3, whatever its shadow record.
const MinRealSuccessesL3 = 3

// minRealSuccesses is MinRealSuccessesL3, or a lower L3 bar (fast
// elevation lowers the whole bar, never only its shadow share).
func minRealSuccesses(th Thresholds) int {
	return min(MinRealSuccessesL3, th.ClassMinSuccessesL3)
}

// shadowCap is the most shadow successes the bar of target counts; -1
// for no cap (L2).
func shadowCap(th Thresholds, target Level) int {
	if target < L3 {
		return -1
	}
	return max(0, th.ClassMinSuccessesL3-minRealSuccesses(th))
}

// countedSuccesses splits a record's successes for target: the counted
// total, real, shadow (capped) and the shadow successes over the cap.
func countedSuccesses(th Thresholds, target Level, r *ClassRecord) (counted, real, shadow,
	over int) {
	if r == nil {
		return 0, 0, 0, 0
	}
	shadow = r.ShadowSuccesses
	real = r.Successes - shadow
	if c := shadowCap(th, target); c >= 0 && shadow > c {
		over, shadow = shadow-c, c
	}
	return real + shadow, real, shadow, over
}

// successSplit renders counted successes with their real/shadow share.
func successSplit(counted, real, shadow, over int) string {
	s := fmt.Sprintf("%d (%d real, %d shadow", counted, real, shadow)
	if over > 0 {
		s += fmt.Sprintf("; %d more shadow over the cap", over)
	}
	return s + ")"
}

// realSuccessesCheck holds L3 to real verified successes.
func realSuccessesCheck(th Thresholds, ev Evidence) Check {
	_, real, _, _ := countedSuccesses(th, L3, ev.Record)
	need := minRealSuccesses(th)
	c := Check{Name: "class_real_successes", Met: real >= need, Observed: fmt.Sprint(real),
		Required: fmt.Sprintf(">= %d real verified successes since the last demerit", need)}
	if !c.Met {
		c.How = fmt.Sprintf("%d more real (executed and verified) %s actions: shadow "+
			"decisions alone never make a class unattended. Approve its one-click "+
			"handoffs and let their verification finish.", need-real, ev.Class)
	}
	return c
}
