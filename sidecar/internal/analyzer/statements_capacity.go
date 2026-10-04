package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
)

// utilityDominated reports a classified pg_stat_statements whose entries
// are at least half utility statements.
func utilityDominated(u *collector.StatStatementsUsage) bool {
	return u != nil && u.Classified && u.Entries > 0 && u.Utility*2 >= u.Entries
}

// usageDetail adds the usage counts that were read to detail and returns
// the title's suffix when utility statements dominate. Unknown counts
// (unclassified texts, no info view) are left out, never zero.
func usageDetail(u *collector.StatStatementsUsage, detail map[string]any) string {
	if u.Classified {
		detail["utility_statements"] = u.Utility
		detail["copy_to_stdout_statements"] = u.CopyOut
	}
	if u.Dealloc >= 0 {
		detail["deallocations"] = u.Dealloc
	}
	if u.TrackUtility != "" {
		detail["track_utility"] = u.TrackUtility
	}
	if !utilityDominated(u) {
		return ""
	}
	return fmt.Sprintf(", %d of them utility statements (%d COPY ... TO stdout)",
		u.Utility, u.CopyOut)
}

// raiseMax is the pg_stat_statements.max advice with its restart caveat.
const raiseMax = "raise pg_stat_statements.max (it takes effect only after a server " +
	"restart, and every entry costs shared memory)"

// capacityAdvice says what to change. Utility statements dominating with
// track_utility on: stop tracking them (a reload, no restart), else raise
// the max. Evictions already happening are cited. It is advice only: a
// server setting is never changed by pg_sage on its own.
func capacityAdvice(u *collector.StatStatementsUsage) string {
	advice := "Review the tracked statements or " + raiseMax + "."
	if utilityDominated(u) && u.TrackUtility != "off" {
		advice = fmt.Sprintf("Set pg_stat_statements.track_utility = off (ALTER SYSTEM, "+
			"then a configuration reload; no restart): %d of %d entries are utility "+
			"statements such as pg_dump's COPY ... TO stdout, which then stop taking "+
			"entries from application queries. Or %s.", u.Utility, u.Entries, raiseMax)
	} else if u != nil {
		advice = "Raise pg_stat_statements.max: it takes effect only after a server " +
			"restart, and every entry costs shared memory. Or review the tracked statements."
	}
	if u != nil && u.Dealloc > 0 {
		advice += fmt.Sprintf(" %d deallocations since the statistics were reset: "+
			"entries are being evicted and their statistics lost.", u.Dealloc)
	}
	return advice
}
