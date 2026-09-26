package briefing

import (
	"context"
	"time"
)

// maxScheduleLookback bounds the scan for a crossed schedule time.
const maxScheduleLookback = 366 * 24 * time.Hour

// crossedSince reports whether a scheduled minute lies in (after, now].
// The minute containing `after` is excluded: a run inside the 06:00
// minute satisfies the 06:00 schedule.
func (s *cronSchedule) crossedSince(after, now time.Time) bool {
	start := after.Truncate(time.Minute).Add(time.Minute)
	if floor := now.Add(-maxScheduleLookback); start.Before(floor) {
		start = floor.Truncate(time.Minute)
	}
	for t := start.In(now.Location()); !t.After(now); {
		switch {
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case !s.hours[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case s.minutes[t.Minute()]:
			return true
		default:
			t = t.Add(time.Minute)
		}
	}
	return false
}

func (s *cronSchedule) dayMatches(t time.Time) bool {
	return s.doms[t.Day()-1] && s.months[t.Month()-1] && s.dows[t.Weekday()]
}

// ShouldRun reports whether a scheduled time was crossed since the last
// run (G3-B04). The orchestrator polls roughly every analyzer interval
// (~10 min), so requiring now to fall inside the scheduled minute made a
// daily briefing fire on ~1 day in 10. The last run is loaded once from
// sage.briefings so a restart across the scheduled time still fires; with
// no history the worker's start time is the anchor. A worker built
// without either (tests) falls back to exact-minute matching.
func (w *Worker) ShouldRun(now time.Time) bool {
	if !w.schedule.valid {
		return false
	}
	w.loadLastRun()
	anchor := w.lastRun
	if anchor.IsZero() {
		anchor = w.startedAt
	}
	if anchor.IsZero() {
		return w.schedule.matches(now)
	}
	return w.schedule.crossedSince(anchor, now)
}

// MarkRan records that a briefing was just generated.
func (w *Worker) MarkRan() {
	w.markRanAt(time.Now())
}

func (w *Worker) markRanAt(t time.Time) {
	w.lastRun = t
	w.lastRunLoaded = true
}

// loadLastRun reads the most recent persisted briefing time once.
func (w *Worker) loadLastRun() {
	if w.lastRunLoaded || w.pool == nil {
		return
	}
	w.lastRunLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var last *time.Time
	err := w.pool.QueryRow(ctx,
		`/* pg_sage */ SELECT max(generated_at) FROM sage.briefings`,
	).Scan(&last)
	if err != nil {
		w.logFn("WARN", "briefing: load last run: %v", err)
		return
	}
	if last != nil && last.After(w.lastRun) {
		w.lastRun = *last
	}
}
