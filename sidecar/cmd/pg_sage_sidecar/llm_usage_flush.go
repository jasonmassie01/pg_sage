package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// usageCounts gathers counters by the UTC day they belong to (Unix
// seconds of its midnight): a counter not rolled over since midnight
// still names the previous day, and is written there.
type usageCounts map[int64]map[usageKey]int64

func (c usageCounts) add(day time.Time, k usageKey, tokens int64) {
	if tokens <= 0 {
		return
	}
	unix := ledgerDay(day).Unix()
	if c[unix] == nil {
		c[unix] = map[usageKey]int64{}
	}
	c[unix][k] = max(c[unix][k], tokens)
}

// collect reads every counter. Several clients behind one key (a retired
// runtime and its replacement) report the highest.
func (l *llmUsageLedger) collect() usageCounts {
	counts := usageCounts{}
	entries, global := l.registry.usageEntries()
	seen := make(map[*llm.Client]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.client] {
			continue
		}
		seen[entry.client] = true
		day, tokens := entry.client.UsageSnapshot()
		counts.add(day, usageKey{l.keys.keyOf(entry.database),
			llmUsageRole(entry.role)}, tokens)
	}
	if global != nil {
		day, used := global.Usage()
		counts.add(day, usageKey{llmUsageProcessKey, llmUsageRoleShared}, used)
	}
	if l.budget != nil {
		day, snapshot := l.budget.SnapshotDay()
		for name, usage := range snapshot {
			counts.add(day, usageKey{l.keys.keyOf(name), llmUsageRoleFleet},
				int64(usage.Used))
		}
	}
	return counts
}

// pending drops today's counters already persisted, so an idle sidecar
// does not rewrite the same rows every tick.
func (l *llmUsageLedger) pending(counts usageCounts) map[int64][]store.LLMUsageRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[int64][]store.LLMUsageRow, len(counts))
	for unix, byKey := range counts {
		today := unix == l.day.Unix()
		for k, tokens := range byKey {
			if today && tokens <= l.high[k] {
				continue
			}
			out[unix] = append(out[unix], store.LLMUsageRow{
				DatabaseKey: k.key, Role: k.role, Tokens: tokens})
		}
	}
	return out
}

// flush writes every counter that grew since the last flush.
func (l *llmUsageLedger) flush(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.rollDay()
	for unix, rows := range l.pending(l.collect()) {
		if err := l.store.Flush(ctx, time.Unix(unix, 0).UTC(), rows); err != nil {
			l.noteFailure(err)
			return err
		}
		l.recordFlushed(unix, rows)
	}
	l.mu.Lock()
	l.flushedAt = l.now()
	l.mu.Unlock()
	l.noteSuccess()
	return nil
}

// rollDay starts a new day's high-water marks after UTC midnight.
func (l *llmUsageLedger) rollDay() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if today := ledgerDay(l.now()); !today.Equal(l.day) {
		l.day, l.high = today, map[usageKey]int64{}
	}
}

func (l *llmUsageLedger) recordFlushed(unix int64, rows []store.LLMUsageRow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if unix != l.day.Unix() {
		return
	}
	for _, row := range rows {
		k := usageKey{row.DatabaseKey, row.Role}
		l.high[k] = max(l.high[k], row.Tokens)
	}
}

// reset runs the admin budget reset (resetLocal) and clears today's rows,
// so a restart does not undo it. A failed clear is logged: the next
// restart today would restore the pre-reset usage.
func (l *llmUsageLedger) reset(ctx context.Context, resetLocal func()) {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	resetLocal()
	day := ledgerDay(l.now())
	l.mu.Lock()
	l.day, l.high = day, map[usageKey]int64{}
	l.mu.Unlock()
	if err := l.store.Clear(ctx, day); err != nil {
		l.warn("LLM budget reset not saved to the control database: %v; a restart "+
			"today restores the usage from before the reset", err)
	}
}

// prune drops rows older than the retention; a failure only waits for the
// next start.
func (l *llmUsageLedger) prune(ctx context.Context) {
	cutoff := ledgerDay(l.now()).Add(-llmUsageRetention)
	if _, err := l.store.Prune(ctx, cutoff); err != nil {
		l.warn("prune LLM usage rows before %s: %v", cutoff.Format(time.DateOnly), err)
	}
}
