package rca

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Log fast path (rca-06). Log signals otherwise become incidents only in
// the analyzer cycle (default 600 s), so a PANIC or disk-full line could
// wait ten minutes to page. A separate ticker takes the critical log
// signals every rca.log_fast_path_interval_seconds (default 5 s) and opens
// or refreshes their incidents at once. Escalation and auto-resolution
// still count analyzer cycles; the next cycle treats a fast-path sighting
// as still firing. Non-critical log signals keep waiting for the cycle.

// SignalLogWatcherOverloaded is the warning a log watcher emits when a
// drain lost lines to queue overflow or the per-cycle classifier cap.
const SignalLogWatcherOverloaded = "log_watcher_overloaded"

// logFastPathTickTimeout bounds one log fast-path tick.
const logFastPathTickTimeout = 30 * time.Second

// CriticalLogSource hands over critical log signals ahead of the analyzer
// cycle, leaving the others for LogSource.Drain.
type CriticalLogSource interface {
	DrainCritical() []*Signal
}

// ObserveLogSignals opens or refreshes the incidents of the critical log
// signals among signals, without counting an analyzer cycle, and returns
// the open incidents it touched. Other signals are ignored. The replay and
// database filters of the analyzer cycle apply. An occurrence is counted
// once per analyzer-cycle window, as the cycle itself would.
func (e *Engine) ObserveLogSignals(
	ctx context.Context, signals []*Signal,
) []Incident {
	if e == nil || len(signals) == 0 {
		return nil
	}
	e.syncResolved(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	critical := criticalSignals(e.filterLogSignals(signals))
	if len(critical) == 0 {
		return nil
	}
	if e.fastFired == nil {
		e.fastFired = make(map[string]bool)
	}
	var touched []Incident
	for _, inc := range e.runLogDecisionTrees(critical) {
		inc.DatabaseName = e.databaseName
		e.merge(&inc, !anyFired(inc.SignalIDs, e.fastFired))
		for _, id := range inc.SignalIDs {
			e.fastFired[id] = true
		}
		if open, ok := e.openByIdentity(identityString(&inc)); ok {
			touched = append(touched, open)
		}
	}
	return touched
}

func criticalSignals(signals []*Signal) []*Signal {
	var out []*Signal
	for _, s := range signals {
		if s != nil && s.Severity == "critical" {
			out = append(out, s)
		}
	}
	return out
}

// LogSignalTicker runs the log fast path for one monitored database.
type LogSignalTicker struct {
	eng      *Engine
	pool     *pgxpool.Pool
	source   CriticalLogSource
	interval time.Duration
	logFn    func(string, string, ...any)
	// retry marks a tick whose incidents were recorded but not persisted.
	retry bool

	// Seams for tests; production uses time.Ticker and the engine.
	newTicker func(time.Duration) (<-chan time.Time, func())
	tick      func(context.Context) error
	hydrate   func(context.Context) error
	persist   func(context.Context) error
}

// NewLogSignalTicker binds the log fast path to an engine, the database's
// pool (its incident store) and a critical log source. interval <= 0 or a
// nil source disables Run.
func NewLogSignalTicker(
	eng *Engine, pool *pgxpool.Pool, source CriticalLogSource,
	interval time.Duration, logFn func(string, string, ...any),
) *LogSignalTicker {
	t := &LogSignalTicker{eng: eng, pool: pool, source: source,
		interval: interval, logFn: logFn, newTicker: realTicker}
	t.tick = t.Tick
	t.hydrate = func(ctx context.Context) error { return eng.Hydrate(ctx, pool) }
	t.persist = func(ctx context.Context) error {
		return eng.PersistIncidents(ctx, pool)
	}
	return t
}

// Interval is the configured log fast-path period (0 = disabled).
func (t *LogSignalTicker) Interval() time.Duration { return t.interval }

// Run ticks until ctx is done. A failed tick is logged and the loop
// continues; it never blocks the analyzer.
func (t *LogSignalTicker) Run(ctx context.Context) {
	if t.interval <= 0 || t.source == nil {
		return
	}
	ticks, stop := t.newTicker(t.interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := t.tick(ctx); err != nil {
				t.logFn("WARN", "rca: log fast path: %v", err)
			}
		}
	}
}

// Tick runs one pass: load incident state, take the critical log signals,
// record them and persist (which also notifies). Hydration comes first so
// a store outage leaves the signals buffered for the next pass or the
// analyzer cycle. A failed persist is retried on the next tick.
func (t *LogSignalTicker) Tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, logFastPathTickTimeout)
	defer cancel()
	if err := t.hydrate(ctx); err != nil {
		return fmt.Errorf("hydrate incident state: %w", err)
	}
	touched := t.eng.ObserveLogSignals(ctx, t.source.DrainCritical())
	if len(touched) == 0 && !t.retry {
		return nil
	}
	if err := t.persist(ctx); err != nil {
		t.retry = true
		return fmt.Errorf("persist incidents: %w", err)
	}
	t.retry = false
	return nil
}
