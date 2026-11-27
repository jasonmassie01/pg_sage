package main

import (
	"context"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// The LLM usage ledger (fleet-10) persists today's token spend of every
// registry client, the global ceiling and the fleet budget to the control
// database, and adds it back at startup, so a restart mid-day (deploy,
// crash loop, /restart) does not hand out the full daily budget again.
// Admission never waits on it: CanSpend and Hold stay in memory, and an
// unreachable ledger only costs the persistence (one warning per streak).

const llmUsageFlushInterval = 60 * time.Second

// llmUsageRetention is how many days of budget counters the ledger keeps
// (the attributed usage of llm-04 keeps llmAttributionRetention).
const llmUsageRetention = 35 * 24 * time.Hour

const (
	llmUsageRoleShared = "shared"
	llmUsageRoleFleet  = "fleet"
)

type llmUsageStore interface {
	Flush(ctx context.Context, day time.Time, rows []store.LLMUsageRow) error
	Load(ctx context.Context, day time.Time) ([]store.LLMUsageRow, error)
	Clear(ctx context.Context, day time.Time) error
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
}

type usageKey struct{ key, role string }

type llmUsageLedger struct {
	store    llmUsageStore
	registry *llmClientRegistry
	budget   *fleet.FleetBudget
	keys     *llmUsageKeys
	now      func() time.Time
	warn     func(format string, args ...any)
	info     func(format string, args ...any)

	flushMu sync.Mutex // serializes flush, restore and reset
	mu      sync.Mutex // guards the fields below
	day     time.Time
	high    map[usageKey]int64 // today's persisted (or restored) counts
	failing bool
	// restoredAt and flushedAt feed /api/v1/llm/status.
	restoredAt, flushedAt time.Time
}

func newLLMUsageLedger(st llmUsageStore, registry *llmClientRegistry,
	budget *fleet.FleetBudget, keys *llmUsageKeys) *llmUsageLedger {
	return &llmUsageLedger{
		store: st, registry: registry, budget: budget, keys: keys,
		now:  time.Now,
		warn: func(f string, a ...any) { logWarn("llm", f, a...) },
		info: func(f string, a ...any) { logInfo("llm", f, a...) },
		high: map[usageKey]int64{},
	}
}

func ledgerDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// restore loads today's rows and adds them to the clients, the global
// ceiling and the fleet budget. Clients registered later start from the
// restored count. A failure leaves accounting local; tick retries.
func (l *llmUsageLedger) restore(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	day := ledgerDay(l.now())
	rows, err := l.store.Load(ctx, day)
	if err != nil {
		l.noteFailure(err)
		return err
	}
	var total int64
	l.mu.Lock()
	l.day, l.high = day, make(map[usageKey]int64, len(rows))
	for _, row := range rows {
		k := usageKey{row.DatabaseKey, row.Role}
		l.high[k] = max(l.high[k], row.Tokens)
		total += row.Tokens
	}
	l.restoredAt = l.now()
	l.mu.Unlock()
	l.registry.attachUsageRestore(l.restoreEntry, func(g *llm.Budget) {
		l.restoreGlobal(day, g)
	})
	l.restoreFleet(rows)
	l.noteSuccess()
	l.info("restored today's LLM usage from the control database: %d counters, "+
		"%d tokens", len(rows), total)
	return nil
}

func (l *llmUsageLedger) restored() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.restoredAt.IsZero()
}

// highFor is today's restored or persisted count for a counter.
func (l *llmUsageLedger) highFor(k usageKey) (time.Time, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !ledgerDay(l.now()).Equal(l.day) {
		return l.day, 0
	}
	return l.day, l.high[k]
}

// restoreEntry runs under the registry lock for every client, existing at
// restore time or built later (llmClientRegistry.restore).
func (l *llmUsageLedger) restoreEntry(entry llmRegistryEntry) {
	k := usageKey{l.keys.keyOf(entry.database), llmUsageRole(entry.role)}
	day, tokens := l.highFor(k)
	if tokens <= 0 {
		return
	}
	if _, err := entry.client.RestoreUsage(day, tokens); err != nil {
		l.warn("restore LLM usage of %s/%s: %v", k.key, k.role, err)
	}
}

func (l *llmUsageLedger) restoreGlobal(day time.Time, global *llm.Budget) {
	_, tokens := l.highFor(usageKey{llmUsageProcessKey, llmUsageRoleShared})
	if tokens <= 0 {
		return
	}
	if _, err := global.RestoreUsage(day, tokens); err != nil {
		l.warn("restore global LLM usage: %v", err)
	}
}

func (l *llmUsageLedger) restoreFleet(rows []store.LLMUsageRow) {
	if l.budget == nil {
		return
	}
	for _, row := range rows {
		if row.Role != llmUsageRoleFleet {
			continue
		}
		name, ok := l.keys.nameOf(row.DatabaseKey)
		if !ok {
			continue // a database this sidecar no longer knows
		}
		if err := l.budget.Restore(name, int(row.Tokens)); err != nil {
			l.warn("restore fleet LLM usage of %q: %v", name, err)
		}
	}
}

// tick retries a failed startup restore, then flushes.
func (l *llmUsageLedger) tick(ctx context.Context) {
	if !l.restored() {
		if err := l.restore(ctx); err != nil {
			return
		}
	}
	_ = l.flush(ctx)
}

func (l *llmUsageLedger) run(ctx context.Context) {
	ticker := time.NewTicker(llmUsageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// status reports when usage was restored and last flushed (zero: never).
func (l *llmUsageLedger) status() (time.Time, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.restoredAt, l.flushedAt
}

func (l *llmUsageLedger) noteFailure(err error) {
	l.mu.Lock()
	first := !l.failing
	l.failing = true
	l.mu.Unlock()
	if first {
		l.warn("LLM usage ledger in the control database is unreachable: %v; "+
			"accounting stays local, so a restart before it recovers restarts "+
			"today's LLM usage from local accounting", err)
	}
}

func (l *llmUsageLedger) noteSuccess() {
	l.mu.Lock()
	recovered := l.failing
	l.failing = false
	l.mu.Unlock()
	if recovered {
		l.info("LLM usage ledger in the control database is reachable again")
	}
}
