package rca

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Lock-chain fast path (Sage SRE M0). The analyzer cycle (default 600 s)
// is too slow to page on a blocking chain, so a separate ticker probes
// lock chains every rca.lock_chain_interval_seconds (default 60 s) and
// opens or updates the lock_contention incident at once. Escalation and
// auto-resolution still count analyzer cycles, so their configured
// meaning is unchanged; a fast-path sighting only defers auto-resolution
// until the next analyzer cycle has seen the chain gone.

// fastPathTickTimeout bounds one fast-path tick (probe + persistence).
const fastPathTickTimeout = 30 * time.Second

// ObserveLockChains records a fast-path lock-chain observation. It opens
// the lock_contention incident or refreshes the open one (last seen,
// severity, backend identities) without counting an analyzer cycle, and
// returns the incident it touched. Nil or quiet findings are a no-op.
func (e *Engine) ObserveLockChains(
	ctx context.Context, findings []analyzer.Finding,
) []Incident {
	if e == nil {
		return nil
	}
	e.syncResolved(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	sig := e.detectLockContention(findings)
	if sig == nil {
		return nil
	}
	inc := lockContentionIncident(sig)
	inc.DatabaseName = e.databaseName
	e.observe(&inc)
	if e.fastFired == nil {
		e.fastFired = make(map[string]bool)
	}
	e.fastFired[sig.ID] = true
	key := identityString(&inc)
	for _, open := range e.activeIncidents() {
		if identityString(&open) == key {
			return []Incident{open}
		}
	}
	return nil
}

// LockChainProbe returns the current lock_chain findings for one
// database (analyzer.ProbeLockChains in production).
type LockChainProbe func(ctx context.Context) ([]analyzer.Finding, error)

// LockChainTicker runs the fast path for one monitored database.
type LockChainTicker struct {
	eng      *Engine
	pool     *pgxpool.Pool
	probe    LockChainProbe
	interval time.Duration
	logFn    func(string, string, ...any)

	// Seams for tests; production uses time.Ticker and the methods below.
	newTicker func(time.Duration) (<-chan time.Time, func())
	tick      func(context.Context) error
	hydrate   func(context.Context) error
}

// NewLockChainTicker binds the fast path to an engine, the database's
// pool (its incident store) and a probe. interval <= 0 disables Run.
func NewLockChainTicker(
	eng *Engine, pool *pgxpool.Pool, probe LockChainProbe,
	interval time.Duration, logFn func(string, string, ...any),
) *LockChainTicker {
	t := &LockChainTicker{eng: eng, pool: pool, probe: probe,
		interval: interval, logFn: logFn, newTicker: realTicker}
	t.tick = t.Tick
	t.hydrate = func(ctx context.Context) error {
		return eng.Hydrate(ctx, pool)
	}
	return t
}

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	tk := time.NewTicker(d)
	return tk.C, tk.Stop
}

// Interval is the configured fast-path period (0 = disabled).
func (t *LockChainTicker) Interval() time.Duration { return t.interval }

// Run ticks until ctx is done. A failed tick is logged and the loop
// continues; it never blocks the analyzer.
func (t *LockChainTicker) Run(ctx context.Context) {
	if t.interval <= 0 {
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
				t.logFn("WARN", "rca: lock-chain fast path: %v", err)
			}
		}
	}
}

// Tick runs one fast-path pass: load incident state, probe lock chains,
// record the observation and persist (which also notifies). Nothing is
// written when no chain is found.
func (t *LockChainTicker) Tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, fastPathTickTimeout)
	defer cancel()
	if err := t.hydrate(ctx); err != nil {
		return fmt.Errorf("hydrate incident state: %w", err)
	}
	if t.probe == nil {
		return errors.New("lock-chain probe not configured")
	}
	findings, err := t.probe(ctx)
	if err != nil {
		return fmt.Errorf("lock-chain probe: %w", err)
	}
	if len(t.eng.ObserveLockChains(ctx, findings)) == 0 {
		return nil
	}
	if err := t.eng.PersistIncidents(ctx, t.pool); err != nil {
		return fmt.Errorf("persist incidents: %w", err)
	}
	return nil
}
