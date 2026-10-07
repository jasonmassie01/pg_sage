package retention

import (
	"sync"
	"time"
)

// A swept rule (purgeRule.sweepCol) reads, on an incremental pass, only
// rows whose sweep column is at or after the floor its last complete pass
// left: rows below it were read then and kept. A full sweep, the first
// pass of the process and one every fullSweepEvery, reads them all, so a
// row whose keep reason went away is purged within a day.
const (
	fullSweepEvery = 24 * time.Hour
	// sweepOverlap moves the floor back to cover the difference between
	// the sidecar's clock and the database's (the window is now() there).
	sweepOverlap = time.Hour
)

// sweepState is a swept rule's progress in this process.
type sweepState struct {
	floor  time.Time // rows below it were read by a complete pass
	fullAt time.Time // the last complete full sweep
}

// sweepPlan returns the floor of the next pass, zero with full set for a
// full sweep.
func sweepPlan(st sweepState, now time.Time) (time.Time, bool) {
	if st.fullAt.IsZero() || st.fullAt.After(now) || now.Sub(st.fullAt) >= fullSweepEvery {
		return time.Time{}, true
	}
	return st.floor, false
}

// sweepDone is the state after a pass at now with a days window: a
// complete pass read every row created before the window's start. An
// unfinished pass (deadline, error) changes nothing. The floor never
// moves back.
func sweepDone(prev sweepState, now time.Time, days int, full, done bool) sweepState {
	if !done {
		return prev
	}
	next := prev
	floor := now.Add(-time.Duration(days)*24*time.Hour - sweepOverlap)
	if floor.After(prev.floor) {
		next.floor = floor
	}
	if full {
		next.fullAt = now
	}
	return next
}

// sweepKey names a swept rule's state.
func sweepKey(rule purgeRule) string { return rule.table + "|" + rule.timeCol }

// sweepBook holds the swept rules' progress in this process.
type sweepBook struct {
	mu     sync.Mutex
	states map[string]sweepState
}

// planSweep returns the floor and kind of rule's next pass.
func (c *Cleaner) planSweep(rule purgeRule) (time.Time, bool) {
	if c.sweeps == nil {
		return time.Time{}, true
	}
	c.sweeps.mu.Lock()
	defer c.sweeps.mu.Unlock()
	return sweepPlan(c.sweeps.states[sweepKey(rule)], c.clock())
}

// finishSweep records rule's pass, started at start.
func (c *Cleaner) finishSweep(rule purgeRule, start time.Time, full, done bool) {
	if c.sweeps == nil {
		return
	}
	c.sweeps.mu.Lock()
	defer c.sweeps.mu.Unlock()
	if c.sweeps.states == nil {
		c.sweeps.states = map[string]sweepState{}
	}
	key := sweepKey(rule)
	c.sweeps.states[key] = sweepDone(c.sweeps.states[key], start, rule.days, full, done)
}

// clock is the cleaner's time source (tests move it).
func (c *Cleaner) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
