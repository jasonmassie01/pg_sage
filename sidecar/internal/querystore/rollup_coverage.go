package querystore

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The coverage report of the long per-query history (evid-01b): how much
// of the database's execution time the rollup speaks for, how often its
// statistics broke, and its size. Unknown is reported, never ok by
// default.

// Coverage statuses.
const (
	RollupCoverageOK           = "ok"
	RollupCoverageLearning     = "learning"      // under minHistory of history
	RollupCoverageMostlyBreaks = "mostly_breaks" // over a quarter of the hours broke
	RollupCoverageUnknown      = "unknown"       // no statement totals to compare with
)

// minHistory is the history a status other than learning needs.
const minHistory = 14 * 24 * time.Hour

// CoverageReport is one database's coverage over a window.
type CoverageReport struct {
	// CoveredExecShare is the sampled share of the statement time over the
	// hours where the database's total is known (nil: unknown).
	CoveredExecShare  *float64
	EpochBreaks       int // hours with a pair that crossed an epoch
	Hours             int // coverage rows in the window
	HoursMissing      int // hours of the window's history span with no row
	DeallocGrowth     *int64
	CountersOnlyHours int // hours without a recorded statements epoch
	OldestHour        *time.Time
	OldestDay         *time.Time
	RollupBytes       int64
	Status            string
}

// coverageSums are the window's sums a status is decided from.
type coverageSums struct {
	knownHours              int
	sampledExecMs, dbExecMs float64
	breakHours              int
	workloadHours           int
	span                    time.Duration // the whole history, oldest to newest hour
}

// coveredShare is sampled / total statement time over the known hours,
// at most 1 (the admitted set may hold statements the totals leave out).
func coveredShare(s coverageSums) *float64 {
	if s.knownHours == 0 || s.dbExecMs <= 0 {
		return nil
	}
	v := math.Min(s.sampledExecMs/s.dbExecMs, 1)
	return &v
}

// classifyCoverage: unknown without a share, mostly_breaks when more than
// a quarter of the hours with workload broke (a compute that suspends),
// learning under two weeks of history, ok otherwise.
func classifyCoverage(s coverageSums) string {
	switch {
	case coveredShare(s) == nil:
		return RollupCoverageUnknown
	case s.workloadHours > 0 && s.breakHours*4 > s.workloadHours:
		return RollupCoverageMostlyBreaks
	case s.span < minHistory:
		return RollupCoverageLearning
	}
	return RollupCoverageOK
}

// RollupCoverage reads db's coverage over [from, to) from its history
// store (histstore.Resolve), with the rollup's size.
func RollupCoverage(ctx context.Context, db any, from, to time.Time) (CoverageReport,
	error) {
	st := histstore.Resolve(db)
	var s coverageSums
	var rep CoverageReport
	var newest *time.Time
	err := st.QueryRow(ctx, coverageSQL, from, to).Scan(&rep.Hours, &s.knownHours,
		&s.sampledExecMs, &s.dbExecMs, &s.breakHours, &s.workloadHours, &rep.DeallocGrowth,
		&rep.CountersOnlyHours, &rep.OldestHour, &newest, &rep.OldestDay)
	if err != nil {
		return CoverageReport{}, fmt.Errorf("read the query rollup coverage in "+
			"sage.query_rollup_coverage: %w", err)
	}
	if rep.OldestHour != nil && newest != nil {
		s.span = newest.Add(time.Hour).Sub(*rep.OldestHour)
		rep.HoursMissing = missingHours(from, to, *rep.OldestHour, *newest, rep.Hours)
	}
	if rep.OldestDay != nil {
		d := rep.OldestDay.UTC()
		rep.OldestDay = &d
	}
	if rep.OldestHour != nil {
		h := rep.OldestHour.UTC()
		rep.OldestHour = &h
	}
	rep.EpochBreaks = s.breakHours
	rep.CoveredExecShare = coveredShare(s)
	rep.Status = classifyCoverage(s)
	if rep.RollupBytes, err = RollupBytes(ctx, db); err != nil {
		return CoverageReport{}, err
	}
	return rep, nil
}

// missingHours is the hours of [from, to) inside the history's span
// (oldest to newest hour) that have no coverage row.
func missingHours(from, to, oldest, newest time.Time, have int) int {
	lo, hi := from, to
	if oldest.After(lo) {
		lo = oldest
	}
	if end := newest.Add(time.Hour); end.Before(hi) {
		hi = end
	}
	if t := lo.Truncate(time.Hour); t.Before(lo) {
		lo = t.Add(time.Hour)
	}
	if !hi.After(lo) {
		return 0
	}
	want := int(math.Ceil(hi.Sub(lo).Hours()))
	return max(want-have, 0)
}

// RollupBytes is the size of db's per-query rollup: the three tables in
// the monitored placement, the database's share of them in a meta store.
func RollupBytes(ctx context.Context, db any) (int64, error) {
	st := histstore.Resolve(db)
	if st.Scoped() {
		return st.ShareBytes(ctx, histstore.RollupTables)
	}
	var n int64
	if err := st.QueryRow(ctx, rollupBytesSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("measure the query rollup tables: %w", err)
	}
	return n, nil
}
