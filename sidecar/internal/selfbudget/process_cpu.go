package selfbudget

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// topLoopCount is how many loops the process CPU finding names.
const topLoopCount = 5

// CPUSink is where the process watch keeps its one finding: on the
// lowest-named live database that accepts it.
type CPUSink interface {
	// Databases are the names of the live monitored databases.
	Databases() []string
	// Raise opens or refreshes the process CPU finding on database.
	Raise(ctx context.Context, database string, b Breach, loops []LoopCost) error
	// Clear resolves database's open process CPU finding, if any.
	Clear(ctx context.Context, database string) error
}

// WatchOptions builds a ProcessCPUWatch. Read is the process CPU counter
// (ProcessCPU in production), Budget the cpu_ms_per_cycle budget (0
// disables), Cycle the global collector interval the rate is per (the
// documented unit) and Loops the tracker whose busiest loops a finding
// names. A nil Now is time.Now; a nil Sink keeps the reading only.
type WatchOptions struct {
	Read   func() (time.Duration, bool)
	Now    func() time.Time
	Budget func() float64
	Cycle  func() time.Duration
	Sink   CPUSink
	Loops  *Loops
}

// CPUReading is the watch's latest observation: the process CPU per
// collector cycle (Known false before a usable window), the budget it was
// compared with, the busiest loops of local work since the previous
// tick, and the database that carries the finding ("" when none does).
type CPUReading struct {
	Known      bool
	MsPerCycle float64
	BudgetMs   float64
	Loops      []LoopCost
	Database   string
}

// ProcessCPUWatch measures the sidecar process's CPU once for every
// monitored database and keeps one sage_self_budget finding for it
// (footprint-07): self_budget.cpu_ms_per_cycle is a process budget.
// Ticks are serialized; Reading is safe to call concurrently.
type ProcessCPUWatch struct {
	meter  *CPUMeter
	budget func() float64
	cycle  func() time.Duration
	sink   CPUSink
	loops  *Loops

	mu        sync.Mutex
	loopsPrev map[string]LoopStat
	// clean names the databases known to hold no open process CPU
	// finding: cleared since the last raise there.
	clean   map[string]bool
	reading atomic.Pointer[CPUReading]
}

// NewProcessCPUWatch builds a watch from o.
func NewProcessCPUWatch(o WatchOptions) *ProcessCPUWatch {
	w := &ProcessCPUWatch{meter: NewCPUMeter(o.Read, o.Now), budget: o.Budget,
		cycle: o.Cycle, sink: o.Sink, loops: o.Loops, clean: map[string]bool{}}
	if w.budget == nil {
		w.budget = func() float64 { return 0 }
	}
	if w.cycle == nil {
		w.cycle = func() time.Duration { return 0 }
	}
	return w
}

// Reading is the latest observation (unknown on a nil watch or before
// the first usable window).
func (w *ProcessCPUWatch) Reading() CPUReading {
	if w == nil {
		return CPUReading{}
	}
	if r := w.reading.Load(); r != nil {
		out := *r
		out.Loops = append([]LoopCost(nil), r.Loops...)
		return out
	}
	return CPUReading{}
}

// Tick observes the CPU since the previous tick. Over budget, the finding
// is raised on the first live database (by name) that accepts it and
// resolved on the others; within budget or disabled, it is resolved
// everywhere. An unknown window changes nothing, so an open finding stays
// open until there is proof. The error joins every refused write.
func (w *ProcessCPUWatch) Tick(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ms, known := w.meter.Observe(w.cycle())
	cur := w.loops.Snapshot()
	top := TopLoops(LocalWork(w.loopsPrev), LocalWork(cur), topLoopCount)
	w.loopsPrev = cur
	r := CPUReading{Known: known, MsPerCycle: ms, BudgetMs: w.budget(), Loops: top}
	if !known {
		w.reading.Store(&r)
		return nil
	}
	var dbs []string
	if w.sink != nil {
		dbs = w.sink.Databases()
	}
	sort.Strings(dbs)
	breaches := Check(Budget{CPUMsPerCycle: r.BudgetMs},
		Usage{CPUKnown: true, CPUMsPerCycle: ms})
	var errs []error
	if len(breaches) > 0 {
		r.Database, errs = w.raise(ctx, dbs, breaches[0], top)
	}
	errs = append(errs, w.clearExcept(ctx, dbs, r.Database)...)
	w.reading.Store(&r)
	return errors.Join(errs...)
}

// raise opens the finding on the first database that accepts it.
func (w *ProcessCPUWatch) raise(ctx context.Context, dbs []string, b Breach,
	top []LoopCost) (string, []error) {
	var errs []error
	for _, db := range dbs {
		err := w.sink.Raise(ctx, db, b, top)
		if err == nil {
			delete(w.clean, db)
			return db, errs
		}
		errs = append(errs, fmt.Errorf("raise the process CPU finding on %s: %w", db, err))
		if ctx.Err() != nil {
			break
		}
	}
	return "", errs
}

// clearExcept resolves the finding on every live database but keep that
// is not known to be clean.
func (w *ProcessCPUWatch) clearExcept(ctx context.Context, dbs []string,
	keep string) []error {
	var errs []error
	for _, db := range dbs {
		if db == keep || w.clean[db] {
			continue
		}
		if err := w.sink.Clear(ctx, db); err != nil {
			errs = append(errs, fmt.Errorf("resolve the process CPU finding on %s: %w",
				db, err))
			continue
		}
		w.clean[db] = true
	}
	return errs
}

// Run ticks every cycle (re-read each time, so a reloaded
// collector.interval_seconds applies) until ctx ends, passing each tick's
// result to report (nil for a tick without a refused write). A cycle of
// zero or less waits a minute.
func (w *ProcessCPUWatch) Run(ctx context.Context, report func(error)) {
	if w == nil {
		return
	}
	for {
		every := w.cycle()
		if every <= 0 {
			every = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		if err := w.Tick(ctx); report != nil {
			report(err)
		}
	}
}

var processWatch atomic.Pointer[ProcessCPUWatch]

// SetProcessWatch registers the process's watch (nil clears it): /metrics
// and the footprint API read the process CPU from it.
func SetProcessWatch(w *ProcessCPUWatch) { processWatch.Store(w) }

// ProcessWatch is the registered watch (nil before one is set).
func ProcessWatch() *ProcessCPUWatch { return processWatch.Load() }
