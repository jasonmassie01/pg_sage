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
	Stop()
}

// pgauditIngester stores correlated pgaudit records.
type pgauditIngester interface {
	Ingest(ctx context.Context, entries []logwatch.LogEntry) (pgaudit.Stats, error)
}

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
	c := pgaudit.NewCorrelator(rt.spec.Pool, rt.spec.Name)
	rt.start(func() {
		runPGAuditCorrelation(rt.ctx, sub, c, logwatchPollInterval(), rt.spec.Scope)
	})
	rt.note("pgaudit_correlation")
}

// runPGAuditCorrelation ingests the subscription's entries every interval
// until ctx ends; a failed ingest is logged and the next batch continues.
func runPGAuditCorrelation(ctx context.Context, sub entryDrainer, c pgauditIngester,
	interval time.Duration, scope string) {
	defer sub.Stop()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		entries := sub.Drain()
		if len(entries) == 0 {
			continue
		}
		if _, err := c.Ingest(ctx, entries); err != nil && ctx.Err() == nil {
			logWarn(scope, "pgaudit correlation: %d log entries not stored: %v",
				len(entries), err)
		}
	}
}
