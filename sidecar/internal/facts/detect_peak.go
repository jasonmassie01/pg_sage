package facts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// The peak-window suggestion (evid-03b): a peak window is something
// pg_sage can measure. SuggestPeakWindows reads 28 days of the hourly
// query rollup (evid-01a) and suggests, in the policy window grammar
// (UTC), the hours of the week whose execution time is in that week's top
// decile in at least 3 of the 4 weeks. It is computed on request and
// never stored: an operator applies it by declaring the owner brief with
// it. With less than 28 days of rollup, or a rollup coverage status
// (evid-01b) other than ok, it suggests nothing and says why.

const (
	peakWeeks        = 4
	peakHoursPerWeek = 7 * 24
	peakHistory      = peakWeeks * peakHoursPerWeek * time.Hour
	// peakDecile is the top decile of a week's 168 hours.
	peakDecile = (peakHoursPerWeek + 9) / 10
	// peakMinWeeks is how many of the 4 weeks an hour must peak in.
	peakMinWeeks = 3
	// peakStatementTimeout bounds each read of the suggestion.
	peakStatementTimeout = "10s"
	peakLockTimeout      = "1s"
)

// PeakHour is one rollup hour a suggestion rests on and its execution time.
type PeakHour struct {
	Hour   time.Time `json:"hour"`
	ExecMs float64   `json:"exec_ms"`
}

// PeakSuggestion is the suggested peak_windows value of the owner brief,
// or none (PeakWindows empty) with the reason. Hours are the top-decile
// rollup hours the suggestion rests on.
type PeakSuggestion struct {
	PeakWindows    string     `json:"peak_windows,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	CoverageStatus string     `json:"coverage_status"`
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	Hours          []PeakHour `json:"hours"`
}

// peakHourlySQL is the execution time of each rollup hour in [$1, $2).
const peakHourlySQL = `/* pg_sage facts */ SELECT h.hour, sum(h.exec_ms)::float8
FROM sage.query_rollup_hourly h WHERE {db:h} AND h.hour >= $1 AND h.hour < $2
GROUP BY h.hour`

// peakOldestHourlySQL is the oldest hour the hourly rollup still holds. Its
// retention (retention.query_rollup_hourly_days, never past snapshots_days)
// can be shorter than the coverage rows' (query_rollup_daily_days).
const peakOldestHourlySQL = `/* pg_sage facts */ SELECT min(h.hour)
FROM sage.query_rollup_hourly h WHERE {db:h}`

// SuggestPeakWindows suggests the peak windows of db (a monitored pool or
// its history store, histstore.Resolve) over the 28 days before now's
// hour. Its reads (SELECTs of pg_sage's own rollup tables) run in one
// transaction bounded by statement and lock timeouts. An error means the
// rollup could not be read; a suggestion of none is never an error.
func SuggestPeakWindows(ctx context.Context, db any, now time.Time) (PeakSuggestion,
	error) {
	to := now.UTC().Truncate(time.Hour)
	from := to.Add(-peakHistory)
	st := histstore.Resolve(db)
	tx, err := st.Begin(ctx)
	if err != nil {
		return PeakSuggestion{}, fmt.Errorf("read the peak-window rollup: %w", err)
	}
	defer rollbackQuietly(ctx, tx)
	if _, err := tx.Exec(ctx, `/* pg_sage facts */ SELECT set_config('statement_timeout',
		$1, true), set_config('lock_timeout', $2, true)`, peakStatementTimeout,
		peakLockTimeout); err != nil {
		return PeakSuggestion{}, fmt.Errorf("bound the peak-window read: %w", err)
	}
	cov, err := querystore.RollupCoverage(ctx, st.WithDB(tx), from, to)
	if err != nil {
		return PeakSuggestion{}, fmt.Errorf("read the peak-window rollup coverage: %w", err)
	}
	none := PeakSuggestion{CoverageStatus: cov.Status, From: from, To: to,
		Hours: []PeakHour{}}
	if cov.OldestHour == nil || cov.OldestHour.After(from) {
		none.Reason = "less than 28 days of hourly rollup" + oldestNote(cov.OldestHour)
		return none, tx.Commit(ctx)
	}
	if cov.Status != querystore.RollupCoverageOK {
		none.Reason = "the hourly rollup's coverage is " + cov.Status + ", not ok"
		return none, tx.Commit(ctx)
	}
	short, err := hourlyHistoryShort(ctx, st, tx, from)
	if err != nil {
		return PeakSuggestion{}, err
	}
	if short != "" {
		none.Reason = short
		return none, tx.Commit(ctx)
	}
	exec, err := readPeakHours(ctx, st, tx, from, to)
	if err != nil {
		return PeakSuggestion{}, err
	}
	got := suggestPeaks(from, exec)
	got.CoverageStatus, got.To = cov.Status, to
	return got, tx.Commit(ctx)
}

func oldestNote(oldest *time.Time) string {
	if oldest == nil {
		return " (none yet)"
	}
	return " (the oldest hour is " + oldest.UTC().Format(time.RFC3339) + ")"
}

// hourlyHistoryShort says why the hourly rollup itself does not reach back
// to from, or "" when it does: the coverage rows outlive the hourly rows,
// and weeks the hourly retention already removed would read as idle.
func hourlyHistoryShort(ctx context.Context, st histstore.Store, tx pgx.Tx,
	from time.Time) (string, error) {
	sql, args := st.Bind(peakOldestHourlySQL)
	var oldest *time.Time
	if err := tx.QueryRow(ctx, sql, args...).Scan(&oldest); err != nil {
		return "", fmt.Errorf("read the peak-window hourly rollup's oldest hour: %w", err)
	}
	if oldest != nil && !oldest.After(from) {
		return "", nil
	}
	return "less than 28 days of hourly rollup" + oldestNote(oldest) +
		"; retention.query_rollup_hourly_days (at most snapshots_days) must keep " +
		"at least 29 days", nil
}

func readPeakHours(ctx context.Context, st histstore.Store, tx pgx.Tx, from,
	to time.Time) (map[time.Time]float64, error) {
	sql, args := st.Bind(peakHourlySQL, from, to)
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("read the peak-window hourly rollup: %w", err)
	}
	defer rows.Close()
	exec := map[time.Time]float64{}
	for rows.Next() {
		var at time.Time
		var ms float64
		if err := rows.Scan(&at, &ms); err != nil {
			return nil, fmt.Errorf("scan the peak-window hourly rollup: %w", err)
		}
		exec[at.UTC()] = ms
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the peak-window hourly rollup: %w", err)
	}
	return exec, nil
}

// suggestPeaks computes the suggestion from the execution time of each
// hour of the 4 weeks from from (hours outside them are ignored; a missing
// hour is idle).
func suggestPeaks(from time.Time, exec map[time.Time]float64) PeakSuggestion {
	out := PeakSuggestion{From: from, To: from.Add(peakHistory), Hours: []PeakHour{}}
	var counts [peakHoursPerWeek]int
	var top [peakWeeks][]PeakHour
	for w := 0; w < peakWeeks; w++ {
		top[w] = weekTopDecile(from.Add(time.Duration(w*peakHoursPerWeek)*time.Hour), exec)
		for _, h := range top[w] {
			counts[hourOfWeek(h.Hour)]++
		}
	}
	var slots []int
	for slot, n := range counts {
		if n >= peakMinWeeks {
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		out.Reason = "no hour of the week was in the top decile of execution time in " +
			"3 of the 4 weeks"
		return out
	}
	windows := formatPeakWindows(slots)
	if len(windows) > maxValueText {
		out.Reason = fmt.Sprintf("the peak hours are too scattered to state within the "+
			"brief's %d characters", maxValueText)
		return out
	}
	out.PeakWindows = windows
	for w := range top {
		for _, h := range top[w] {
			if counts[hourOfWeek(h.Hour)] >= peakMinWeeks {
				out.Hours = append(out.Hours, h)
			}
		}
	}
	sort.Slice(out.Hours, func(i, j int) bool {
		return out.Hours[i].Hour.Before(out.Hours[j].Hour)
	})
	return out
}

// weekTopDecile returns the hours of the week from start in its top
// decile: busy (exec > 0) and with at most peakDecile hours of the week at
// least as busy, so ties at the boundary are left out.
func weekTopDecile(start time.Time, exec map[time.Time]float64) []PeakHour {
	hours := make([]PeakHour, 0, peakHoursPerWeek)
	for i := 0; i < peakHoursPerWeek; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		hours = append(hours, PeakHour{Hour: at, ExecMs: exec[at]})
	}
	var out []PeakHour
	for _, h := range hours {
		if h.ExecMs <= 0 {
			continue
		}
		atLeast := 0
		for _, o := range hours {
			if o.ExecMs >= h.ExecMs {
				atLeast++
			}
		}
		if atLeast <= peakDecile {
			out = append(out, h)
		}
	}
	return out
}

// hourOfWeek numbers an hour from Monday 00:00 UTC (0) to Sunday 23:00.
func hourOfWeek(t time.Time) int {
	t = t.UTC()
	return ((int(t.Weekday())+6)%7)*24 + t.Hour()
}

// peakDays are the day names of the window grammar, Monday first.
var peakDays = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// formatPeakWindows states hour-of-week slots as windows of the policy
// grammar: each day's runs of hours, the days of one run joined ("mon,tue
// 09:00-12:00 UTC"), in order of start hour; a run to midnight ends at
// 00:00 and a whole day is the day alone.
func formatPeakWindows(slots []int) string {
	type run struct{ start, end int }
	days := map[run][]string{}
	var order []run
	byDay := [7][24]bool{}
	for _, s := range slots {
		byDay[s/24][s%24] = true
	}
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; {
			if !byDay[d][h] {
				h++
				continue
			}
			r := run{start: h}
			for h < 24 && byDay[d][h] {
				h++
			}
			r.end = h
			if days[r] == nil {
				order = append(order, r)
			}
			days[r] = append(days[r], peakDays[d])
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].start < order[j].start ||
			(order[i].start == order[j].start && order[i].end < order[j].end)
	})
	parts := make([]string, 0, len(order))
	for _, r := range order {
		dayList := strings.Join(days[r], ",")
		if r.start == 0 && r.end == 24 {
			parts = append(parts, dayList+" UTC")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %02d:00-%02d:00 UTC", dayList, r.start,
			r.end%24))
	}
	return strings.Join(parts, "; ")
}
