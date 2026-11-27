package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/pgaudit"
)

// entryDrainer is a parsed-log subscription.
type entryDrainer interface {
	Drain() []logwatch.LogEntry
	TakeEvicted() int
	Stop()
}

// pgauditIngester stores correlated pgaudit records.
type pgauditIngester interface {
	Ingest(ctx context.Context, entries []logwatch.LogEntry) (pgaudit.Stats, error)
	Pending() int
}

// A batch the store refused is sent again on the next ticks, at most
// pgauditMaxRetries times and at most its newest pgauditMaxRetryEntries.
const (
	pgauditMaxRetries      = 3
	pgauditMaxRetryEntries = 10_000
)

// startPGAuditCorrelation keeps the pgaudit records of pg_sage's actions
// and of agent principals (E2, §6.17) when pgaudit is preloaded and the
// server log is readable through logwatch.
func (rt *databaseRuntime) startPGAuditCorrelation() {
	if rt.cfg == nil || !rt.cfg.Audit.PGAudit.Correlate {
		return
	}
	ctx, cancel := context.WithTimeout(rt.ctx, 5*time.Second)
	on, err := pgaudit.Installed(ctx, rt.spec.Pool)
	cancel()
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: pgaudit correlation skipped: %v", rt.spec.Name, err)
		return
	}
	if !on {
		return
	}
	if rt.logFanout == nil {
		logInfo(rt.spec.Scope, "db %q: pgaudit is loaded but pg_sage cannot read the "+
			"server log; enable logwatch to correlate its records", rt.spec.Name)
		return
	}
	sub := rt.logFanout.SubscribeEntries("pgaudit:"+rt.spec.Name, rt.spec.Config.Database)
	c := pgaudit.NewCorrelator(rt.spec.Pool, rt.spec.Name, pgauditRoleCache())
	rt.start(func() {
		runPGAuditCorrelation(rt.ctx, sub, c, logwatchPollInterval(), rt.spec.Name,
			rt.spec.Scope)
	})
	rt.note("pgaudit_correlation")
}

// runPGAuditCorrelation ingests the subscription's entries every interval
// until ctx ends.
func runPGAuditCorrelation(ctx context.Context, sub entryDrainer, c pgauditIngester,
	interval time.Duration, database, scope string) {
	defer sub.Stop()
	l := newPGAuditLoop(sub, c, database, scope)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		l.tick(ctx)
	}
}

// pgauditLoop is one database's correlation loop and its retry state.
type pgauditLoop struct {
	sub      entryDrainer
	c        pgauditIngester
	database string
	scope    string
	retry    []logwatch.LogEntry // the last failed batch, sent again first
	attempts int                 // consecutive failed ingests
}

func newPGAuditLoop(sub entryDrainer, c pgauditIngester, database, scope string) *pgauditLoop {
	return &pgauditLoop{sub: sub, c: c, database: database, scope: scope}
}

// tick ingests the failed batch and the new entries, or only retries the
// correlator's held records, and counts every record's outcome.
func (l *pgauditLoop) tick(ctx context.Context) {
	pgaudit.CountRecords(l.database, pgaudit.OutcomeEvicted, l.sub.TakeEvicted())
	entries := append(l.retry, l.sub.Drain()...)
	l.retry = nil
	if len(entries) == 0 && l.c.Pending() == 0 {
		return
	}
	st, err := l.c.Ingest(ctx, entries)
	if err != nil {
		l.failed(ctx, entries, err)
		return
	}
	l.attempts = 0
	pgaudit.CountRecords(l.database, pgaudit.OutcomeStored, st.Stored)
	pgaudit.CountRecords(l.database, pgaudit.OutcomeUnmatched, st.Dropped+st.Unmatched)
	pgaudit.CountRecords(l.database, pgaudit.OutcomeDropped, st.Overflow)
}

// failed keeps a refused batch for the next tick, its newest
// pgauditMaxRetryEntries only, until pgauditMaxRetries retries have
// failed too; then the batch is dropped. Every loss is counted.
func (l *pgauditLoop) failed(ctx context.Context, entries []logwatch.LogEntry, err error) {
	if ctx.Err() != nil {
		return // the runtime is stopping; nothing will retry
	}
	l.attempts++
	if l.attempts > pgauditMaxRetries {
		pgaudit.CountRecords(l.database, pgaudit.OutcomeDropped, len(entries))
		logWarn(l.scope, "pgaudit correlation: %d log entries dropped after %d failed "+
			"attempts to store them: %v", len(entries), l.attempts, err)
		l.attempts = 0
		return
	}
	if over := len(entries) - pgauditMaxRetryEntries; over > 0 {
		pgaudit.CountRecords(l.database, pgaudit.OutcomeDropped, over)
		entries = entries[over:]
	}
	pgaudit.CountRecords(l.database, pgaudit.OutcomeRequeued, len(entries))
	l.retry = entries
	logWarn(l.scope, "pgaudit correlation: %d log entries not stored (attempt %d of %d), "+
		"retrying next tick: %v", len(entries), l.attempts, pgauditMaxRetries+1, err)
}
