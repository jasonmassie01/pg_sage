package briefing

import (
	"fmt"
	"time"
)

// Window is the time a briefing covers: from the previous briefing to now.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// defaultWindow is the span used when neither a previous briefing nor a
// previous schedule fire is known.
const defaultWindow = 24 * time.Hour

// windowFor is the window of a briefing generated at now (llm-06): it
// starts at the previous briefing's generated_at; on the first run, at the
// schedule's fire before the current one; otherwise 24 hours back.
func (w *Worker) windowFor(now time.Time) Window {
	w.loadLastRun()
	win := Window{Start: now.Add(-defaultWindow), End: now}
	if !w.lastRun.IsZero() {
		if w.lastRun.Before(now) {
			win.Start = w.lastRun
		}
		return win
	}
	sched := w.settings().schedule
	if !sched.valid {
		return win
	}
	current, ok := sched.lastFireAtOrBefore(now)
	if !ok {
		return win
	}
	if previous, ok := sched.lastFireAtOrBefore(current.Add(-time.Minute)); ok {
		win.Start = previous
	}
	return win
}

// lastFireAtOrBefore is the latest scheduled minute at or before t, within
// maxScheduleLookback.
func (s *cronSchedule) lastFireAtOrBefore(t time.Time) (time.Time, bool) {
	floor := t.Add(-maxScheduleLookback)
	for m := t.Truncate(time.Minute); !m.Before(floor); {
		switch {
		case !s.dayMatches(m):
			m = time.Date(m.Year(), m.Month(), m.Day(), 0, 0, 0, 0, m.Location()).
				Add(-time.Minute)
		case !s.hours[m.Hour()]:
			m = time.Date(m.Year(), m.Month(), m.Day(), m.Hour(), 0, 0, 0, m.Location()).
				Add(-time.Minute)
		case s.minutes[m.Minute()]:
			return m, true
		default:
			m = m.Add(-time.Minute)
		}
	}
	return time.Time{}, false
}

// label names the window in the structured briefing: "24h", "7d", "3h", or
// "since <start>" for a span that is not a whole number of hours.
func (win Window) label() string {
	if win.Start.IsZero() || win.End.IsZero() {
		return "24h"
	}
	span := win.End.Sub(win.Start).Round(time.Minute)
	switch {
	case span == defaultWindow:
		return "24h"
	case span > 0 && span%defaultWindow == 0:
		return fmt.Sprintf("%dd", span/defaultWindow)
	case span > 0 && span%time.Hour == 0:
		return fmt.Sprintf("%dh", span/time.Hour)
	}
	return "since " + win.Start.In(win.End.Location()).Format("2006-01-02 15:04 MST")
}
