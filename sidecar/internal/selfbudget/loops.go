package selfbudget

import (
	"sort"
	"sync"
	"time"
)

// Loops accumulates the busy (wall) time, runs, failed runs and last
// successful run of pg_sage's periodic loops, so a budget breach can name
// the loops that worked most and /metrics shows a loop that keeps failing
// (observability-06). Busy time is wall time, not CPU: Go does not
// attribute CPU to goroutines. A loop is keyed component:database
// (component alone for a process loop); the time a run waited on the LLM,
// and the time of runs nested inside it, are not its busy time
// (footprint-07). Safe for concurrent use.
type Loops struct {
	now   func() time.Time
	mu    sync.Mutex
	stats map[string]LoopStat
}

// LoopStat is a loop's cumulative busy time, runs and failed runs, and
// when its last successful run ended (zero until one succeeds).
type LoopStat struct {
	Busy        time.Duration
	Runs        int64
	Errors      int64
	LastSuccess time.Time
}

// LoopCost is a loop's work between two snapshots.
type LoopCost struct {
	Name   string
	BusyMs float64
	Runs   int64
}

// NewLoops returns an empty tracker on the wall clock.
func NewLoops() *Loops { return NewLoopsWithClock(time.Now) }

// NewLoopsWithClock returns an empty tracker that reads time with now
// (time.Now when nil).
func NewLoopsWithClock(now func() time.Time) *Loops {
	if now == nil {
		now = time.Now
	}
	return &Loops{now: now, stats: map[string]LoopStat{}}
}

var process = NewLoops()

// Process is the process-wide tracker the sidecar's loops report to.
func Process() *Loops { return process }

// Add records one successful run of name that was busy for d.
func (l *Loops) Add(name string, d time.Duration) { l.record(name, d, nil) }

// record records one run of name that was busy for d and failed with err
// (nil: it succeeded, and ended now).
func (l *Loops) record(name string, d time.Duration, err error) {
	if l == nil {
		return
	}
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats[name]
	s.Busy += d
	s.Runs++
	if err != nil {
		s.Errors++
	} else {
		s.LastSuccess = now
	}
	l.stats[name] = s
}

// Track starts a run of name that reports no failure; calling the result
// ends it as a success.
func (l *Loops) Track(name string) func() {
	done := l.TrackErr(name)
	return func() { done(nil) }
}

// TrackErr starts a run of name; calling the result with the run's error
// ends it: nil is a success, anything else a failed run.
func (l *Loops) TrackErr(name string) func(err error) {
	start := l.clock()
	return func(err error) { l.record(name, l.clock().Sub(start), err) }
}

// clock is the tracker's time (the wall clock for a nil tracker).
func (l *Loops) clock() time.Time {
	if l == nil || l.now == nil {
		return time.Now()
	}
	return l.now()
}

// Snapshot copies every loop's counters.
func (l *Loops) Snapshot() map[string]LoopStat {
	out := map[string]LoopStat{}
	if l == nil {
		return out
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.stats {
		out[k] = v
	}
	return out
}

// TopLoops are the n loops that were busiest between prev and cur. A loop
// new in cur, or whose counters went back, counts from zero; a loop that
// did not run is left out.
func TopLoops(prev, cur map[string]LoopStat, n int) []LoopCost {
	if n <= 0 {
		return nil
	}
	var out []LoopCost
	for name, c := range cur {
		p, ok := prev[name]
		if !ok || c.Busy < p.Busy || c.Runs < p.Runs {
			p = LoopStat{}
		}
		if runs := c.Runs - p.Runs; runs > 0 {
			out = append(out, LoopCost{Name: name, Runs: runs,
				BusyMs: float64(c.Busy-p.Busy) / float64(time.Millisecond)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BusyMs != out[j].BusyMs {
			return out[i].BusyMs > out[j].BusyMs
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
