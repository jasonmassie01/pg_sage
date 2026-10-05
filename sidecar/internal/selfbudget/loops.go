package selfbudget

import (
	"sort"
	"sync"
	"time"
)

// Loops accumulates the busy (wall) time and runs of pg_sage's periodic
// loops, so a budget breach can name the loops that worked most. Busy
// time is wall time, not CPU: Go does not attribute CPU to goroutines.
// Safe for concurrent use.
type Loops struct {
	mu    sync.Mutex
	stats map[string]LoopStat
}

// LoopStat is a loop's cumulative busy time and runs.
type LoopStat struct {
	Busy time.Duration
	Runs int64
}

// LoopCost is a loop's work between two snapshots.
type LoopCost struct {
	Name   string
	BusyMs float64
	Runs   int64
}

// NewLoops returns an empty tracker.
func NewLoops() *Loops { return &Loops{stats: map[string]LoopStat{}} }

var process = NewLoops()

// Process is the process-wide tracker the sidecar's loops report to.
func Process() *Loops { return process }

// Add records one run of name that was busy for d.
func (l *Loops) Add(name string, d time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats[name]
	s.Busy += d
	s.Runs++
	l.stats[name] = s
}

// Track starts a run of name; calling the result ends it.
func (l *Loops) Track(name string) func() {
	start := time.Now()
	return func() { l.Add(name, time.Since(start)) }
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
