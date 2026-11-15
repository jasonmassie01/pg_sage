package querystore

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// backfillHoursPerPass bounds one pass: the first rollup of 90 days of
// snapshots takes 90 passes, one every rollupTick.
const backfillHoursPerPass = 24

// rollupTick is how often the worker runs a pass.
const rollupTick = 5 * time.Minute

// rollupDueAfter is how far into an hour the previous one is rolled: late
// cycles of a slow collector have landed by then.
const rollupDueAfter = 5 * time.Minute

// RollupWorker keeps one monitored database's hourly rollup current: it
// backfills every closed hour from the oldest queries snapshot, 24 hours
// per pass, then rolls hour h-1 at :05 and re-rolls h-2 (late snapshots).
// Store is the monitored pool (or a histstore.Store); Database names the
// metric series. It writes only sage history tables.
type RollupWorker struct {
	Store    any
	Database string
	Clock    func() time.Time
	Log      func(level, format string, args ...any)
	// Tick overrides rollupTick (tests).
	Tick time.Duration
}

func (w *RollupWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock()
	}
	return time.Now()
}

// Run runs a pass every tick until ctx ends. A failed pass is logged and
// retried on the next tick from the same hour.
func (w *RollupWorker) Run(ctx context.Context) {
	tick := w.Tick
	if tick <= 0 {
		tick = rollupTick
	}
	for {
		if _, err := w.Pass(ctx); err != nil && ctx.Err() == nil && w.Log != nil {
			w.Log("WARN", "query rollup of %s: %v", w.Database, err)
		}
		timer := time.NewTimer(tick)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// lastDueHour is the newest hour due at now: the previous one from :05.
func lastDueHour(now time.Time) time.Time {
	h := now.UTC().Truncate(time.Hour)
	if now.Sub(h) < rollupDueAfter {
		return h.Add(-2 * time.Hour)
	}
	return h.Add(-time.Hour)
}

// passHours are the hours one pass rolls, from next (the first hour not
// rolled) to last (lastDueHour): at most backfillHoursPerPass; a pass that
// rolls only last also re-rolls the hour before it, first.
func passHours(next, last time.Time) []time.Time {
	if next.After(last) {
		return nil
	}
	if next.Equal(last) {
		return []time.Time{last.Add(-time.Hour), last}
	}
	var out []time.Time
	for h := next; !h.After(last) && len(out) < backfillHoursPerPass; h = h.Add(time.Hour) {
		out = append(out, h)
	}
	return out
}

// Pass rolls the hours due now and returns how many it rolled. It stops
// at the first failed hour, which the next pass starts from.
func (w *RollupWorker) Pass(ctx context.Context) (int, error) {
	last := lastDueHour(w.now())
	next, ok, err := w.resume(ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		setRollupLag(w.Database, 0)
		return 0, nil
	}
	rolled := 0
	for _, h := range passHours(next, last) {
		rep, err := RollupHour(ctx, w.Store, h)
		if err != nil {
			noteRollupHour(w.Database, "failed")
			setRollupLag(w.Database, last.Sub(h).Hours()+1)
			return rolled, err
		}
		rolled++
		if h.After(next) || h.Equal(next) {
			next = h.Add(time.Hour)
		}
		result := "ok"
		if rep.Empty() {
			result = "empty"
		}
		noteRollupHour(w.Database, result)
	}
	setRollupLag(w.Database, max(last.Sub(next).Hours()+1, 0))
	return rolled, nil
}

// resume is the first hour not rolled: after the newest rolled hour, or
// the oldest queries snapshot's hour. ok is false without any snapshot.
func (w *RollupWorker) resume(ctx context.Context) (time.Time, bool, error) {
	var newest, oldest *time.Time
	if err := histstore.Resolve(w.Store).QueryRow(ctx, rollupResumeSQL).Scan(&newest,
		&oldest); err != nil {
		return time.Time{}, false, fmt.Errorf("query rollup of %s: find the first hour "+
			"to roll in sage.query_rollup_coverage and sage.snapshots: %w", w.Database, err)
	}
	switch {
	case newest != nil:
		return newest.UTC().Add(time.Hour), true, nil
	case oldest != nil:
		return oldest.UTC().Truncate(time.Hour), true, nil
	}
	return time.Time{}, false, nil
}
