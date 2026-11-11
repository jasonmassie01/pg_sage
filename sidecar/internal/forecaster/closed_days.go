package forecaster

import (
	"math"
	"sort"
	"time"
)

// The daily rules read closed calendar days only (forecast-08). Today's
// partial day is left out, so a rule gives the same answer at 00:10 and at
// 23:50; a day the collector watched for less than minDayCoverage of its
// hours is uncovered; and a day without any snapshot is marked missing
// rather than closed up, so a rate or a week never silently spans it.

// minDayCoverage is the share of a day's hours its snapshots must cover.
const minDayCoverage = 0.8

// coverageTolerance absorbs float error at exactly minDayCoverage.
const coverageTolerance = 1e-9

// dayRow is what a daily read knows about one calendar day of snapshots
// (session time zone): its start, how many snapshots it holds, and the
// first and last of them.
type dayRow struct {
	Day             time.Time
	Samples         int64
	FirstAt, LastAt time.Time
}

// Day is one closed calendar day of a daily read.
type Day struct {
	// Day is the day's start; a missing day's is estimated as today less
	// Offset days of 24 hours.
	Day time.Time
	// Offset is the calendar days before today: 1 is yesterday.
	Offset int
	// Row is the day's index in the rows, -1 for a missing day.
	Row int
	// Coverage is the share of the day's 24 hours its snapshots cover.
	Coverage float64
	Covered  bool
}

// closedDays returns the calendar from the oldest row's day to yesterday,
// oldest first. today is the session's current day start; rows of today
// or later are left out. interval is the collector interval: each
// snapshot covers one interval, capped at the hours between the day's
// first and last snapshot plus one interval, so a burst does not cover a
// day. The interval used is at least the median spacing of the closed
// days' snapshots, so a raised collector.interval_seconds does not
// uncover every day. A zero interval measures no coverage: every day with
// a row is covered.
func closedDays(rows []dayRow, today time.Time, interval time.Duration) []Day {
	byOffset := map[int]int{}
	oldest := 0
	for i, r := range rows {
		off := dayOffset(r.Day, today)
		if off < 1 {
			continue
		}
		if _, dup := byOffset[off]; !dup {
			byOffset[off] = i
		}
		oldest = max(oldest, off)
	}
	interval = coverageInterval(rows, byOffset, interval)
	days := make([]Day, 0, oldest)
	for off := oldest; off >= 1; off-- {
		i, ok := byOffset[off]
		if !ok {
			days = append(days, Day{Day: today.Add(-time.Duration(off) * 24 * time.Hour),
				Offset: off, Row: -1})
			continue
		}
		cov := dayCoverage(rows[i], interval)
		days = append(days, Day{Day: rows[i].Day, Offset: off, Row: i, Coverage: cov,
			Covered: cov >= minDayCoverage-coverageTolerance})
	}
	return days
}

// dayOffset is the calendar days from day to today. Rounding absorbs the
// 23 and 25 hour days of a DST change.
func dayOffset(day, today time.Time) int {
	return int(math.Round(today.Sub(day).Hours() / 24))
}

// coverageInterval is the larger of the configured interval and the
// median spacing of the closed days' snapshots; 0 when none is configured.
func coverageInterval(rows []dayRow, closed map[int]int, configured time.Duration) time.Duration {
	if configured <= 0 {
		return 0
	}
	var spacings []time.Duration
	for _, i := range closed {
		if r := rows[i]; r.Samples >= 2 && r.LastAt.After(r.FirstAt) {
			spacings = append(spacings, r.LastAt.Sub(r.FirstAt)/time.Duration(r.Samples-1))
		}
	}
	if len(spacings) == 0 {
		return configured
	}
	sort.Slice(spacings, func(a, b int) bool { return spacings[a] < spacings[b] })
	return max(configured, spacings[len(spacings)/2])
}

// dayCoverage is the share of 24 hours the row's snapshots cover.
func dayCoverage(r dayRow, interval time.Duration) float64 {
	if interval <= 0 {
		return 1
	}
	covered := time.Duration(r.Samples) * interval
	if span := r.LastAt.Sub(r.FirstAt) + interval; span < covered {
		covered = span
	}
	return math.Max(0, covered.Hours()/24)
}

// closedSystemDays returns the closed days of a system read. An uncovered
// day keeps its place with every input unknown, so the rules skip it as
// they skip an unmeasured day (forecast-07); a missing day has no entry,
// and the rules measure spans and rates from the days' dates.
func closedSystemDays(aggs []DaySystemAgg, today time.Time, interval time.Duration) []DaySystemAgg {
	rows := make([]dayRow, len(aggs))
	for i, a := range aggs {
		rows[i] = dayRow{Day: a.Day, Samples: a.Samples, FirstAt: a.FirstAt, LastAt: a.LastAt}
	}
	var out []DaySystemAgg
	for _, d := range closedDays(rows, today, interval) {
		switch {
		case d.Row < 0:
			continue
		case d.Covered:
			out = append(out, aggs[d.Row])
		default:
			out = append(out, uncoveredSystemDay(aggs[d.Row]))
		}
	}
	return out
}

// uncoveredSystemDay is a day whose every input is unknown.
func uncoveredSystemDay(a DaySystemAgg) DaySystemAgg {
	return DaySystemAgg{Day: a.Day, Samples: a.Samples, FirstAt: a.FirstAt, LastAt: a.LastAt,
		DBSizeUnknown: true, BackendsUnknown: true, MaxConnectionsUnknown: true,
		CheckpointsUnknown: true, CacheUnknown: true}
}
