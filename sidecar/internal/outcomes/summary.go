package outcomes

import (
	"sort"
	"time"
)

// Summarize counts rows. Verified rows were judged on a measurement
// (improved, no change, regressed); inconclusive ones were judged without
// one (inconclusive, unverifiable, expired).
func Summarize(rows []Row) Summary {
	var s Summary
	for _, r := range rows {
		s.Changes++
		switch {
		case measuredVerdict(r.Verdict):
			s.Verified++
		case r.Verdict == VerdictPending:
			s.Pending++
		default:
			s.Inconclusive++
		}
		if r.RollbackState == RollbackRolledBack {
			s.Reverted++
		}
		if r.Measured != nil {
			s.Measured++
		}
	}
	if s.Changes > 0 {
		share := round(float64(s.Measured)/float64(s.Changes), 3)
		s.MeasuredShare = &share
	}
	return s
}

// Merge combines per-database reads into one, newest change first (then
// by database and id), keeping limit rows; the summary covers every row
// read and the errors of every part.
func Merge(limit int, parts ...Result) Result {
	out := Result{Rows: []Row{}, Errors: []ReadError{}}
	for _, p := range parts {
		out.Rows = append(out.Rows, p.Rows...)
		out.Errors = append(out.Errors, p.Errors...)
		out.Truncated = out.Truncated || p.Truncated
		if out.Since == "" {
			out.Since = p.Since
		}
	}
	sort.SliceStable(out.Rows, func(i, j int) bool { return newer(out.Rows[i], out.Rows[j]) })
	out.Summary = Summarize(out.Rows)
	if limit >= 0 && len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
	}
	return out
}

func newer(a, b Row) bool {
	if !a.ExecutedAt.Equal(b.ExecutedAt) {
		return a.ExecutedAt.After(b.ExecutedAt)
	}
	if a.Database != b.Database {
		return a.Database < b.Database
	}
	return a.ActionID > b.ActionID
}

// filterVerdict keeps the rows with verdict ("" keeps every row).
func filterVerdict(rows []Row, verdict string) []Row {
	if verdict == "" {
		return rows
	}
	kept := []Row{}
	for _, r := range rows {
		if r.Verdict == verdict {
			kept = append(kept, r)
		}
	}
	return kept
}

// cutoff is the oldest executed_at a window over now includes.
func cutoff(now time.Time, since time.Duration) time.Time { return now.Add(-since) }
