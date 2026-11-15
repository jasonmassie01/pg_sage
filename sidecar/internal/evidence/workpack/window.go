package workpack

import (
	"fmt"
	"time"
)

// Window is [From, To) on whole UTC hours.
type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// UTC is the window with both instants in UTC.
func (w Window) UTC() Window { return Window{w.From.UTC(), w.To.UTC()} }

// Validate refuses a window that is not [from, to) on whole hours, from
// 14 to 100 days long, with ErrWindow naming the bound.
func (w Window) Validate() error {
	switch {
	case w.From.IsZero() || w.To.IsZero():
		return fmt.Errorf("%w: give both from and to", ErrWindow)
	case !w.From.Before(w.To):
		return fmt.Errorf("%w: from (%s) must be before to (%s)", ErrWindow,
			w.From.UTC().Format(time.RFC3339), w.To.UTC().Format(time.RFC3339))
	case !onHour(w.From) || !onHour(w.To):
		return fmt.Errorf("%w: from and to must be whole UTC hours, got %s and %s",
			ErrWindow, w.From.UTC().Format(time.RFC3339Nano), w.To.UTC().Format(time.RFC3339Nano))
	case w.To.Sub(w.From) < MinWindow:
		return fmt.Errorf("%w: the window is %v; at least 14 days (every hour-of-week "+
			"bucket twice)", ErrWindow, w.To.Sub(w.From))
	case w.To.Sub(w.From) > MaxWindow:
		return fmt.Errorf("%w: the window is %v; at most 100 days", ErrWindow,
			w.To.Sub(w.From))
	}
	return nil
}

func onHour(t time.Time) bool { return t.UTC().Truncate(time.Hour).Equal(t) }

// DefaultWindowEnding is the 28 days ending at now's hour.
func DefaultWindowEnding(now time.Time) Window {
	to := now.UTC().Truncate(time.Hour)
	return Window{to.Add(-DefaultWindow), to}
}

// Granularities of a window day.
const (
	granularityHourly = "hourly"
	granularityDaily  = "daily"
)

// Day is one UTC day the window touches: whether it is read from the
// hourly or the daily tier, how many of its hours are in the window, and
// whether it counts (a daily day counts only when the window holds all 24
// of its hours: the daily tier cannot be split).
type Day struct {
	Day         string `json:"day"`
	Granularity string `json:"granularity"`
	Hours       int    `json:"hours"`
	Counted     bool   `json:"counted"`
}

// dayPlan is which part of the window each tier serves.
type dayPlan struct {
	days       []Day
	dailyDays  []string  // counted daily days, oldest first
	hourlyFrom time.Time // the hourly tier serves [hourlyFrom, To)
}

// planDays splits w at the hourly rollup's retention: whole UTC days
// before dayStart(now) - hourlyDays read the daily tier.
func planDays(w Window, hourlyDays int, now time.Time) dayPlan {
	w = w.UTC()
	cutoff := time.Time{}
	if hourlyDays > 0 {
		cutoff = dayStart(now.UTC()).AddDate(0, 0, -hourlyDays)
	}
	p := dayPlan{hourlyFrom: w.From}
	if cutoff.After(w.From) {
		p.hourlyFrom = cutoff
		if cutoff.After(w.To) {
			p.hourlyFrom = w.To
		}
	}
	for d := dayStart(w.From); d.Before(w.To); d = d.AddDate(0, 0, 1) {
		lo, hi := maxTime(d, w.From), minTime(d.AddDate(0, 0, 1), w.To)
		e := Day{Day: d.Format(time.DateOnly), Granularity: granularityHourly,
			Hours: int(hi.Sub(lo) / time.Hour), Counted: true}
		if d.Before(cutoff) {
			e.Granularity = granularityDaily
			e.Counted = e.Hours == 24
			if e.Counted {
				p.dailyDays = append(p.dailyDays, e.Day)
			}
		}
		p.days = append(p.days, e)
	}
	return p
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
