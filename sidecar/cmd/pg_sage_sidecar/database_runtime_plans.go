package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
)

// startObservedPlans stores the executed plans auto_explain writes to this
// database's server log (rca-04), from the cluster's log watcher: one
// subscriber per database, so fleet databases sharing a server each keep
// their own plans. It reports whether ingestion started.
func (rt *databaseRuntime) startObservedPlans() bool {
	if rt.cfg == nil || !rt.cfg.AutoExplain.Enabled || rt.logFanout == nil {
		return false
	}
	db, pool := rt.spec.Config.Database, rt.spec.Pool
	sub := rt.logFanout.SubscribeEntries("plans:"+rt.spec.Name, db)
	sink := autoexplain.ObservedPlanSink{Database: db,
		MaxPerCycle: observedPlanCap(rt.cfg.AutoExplain.MaxPlansPerCycle),
		LogFn:       logStructuredWrapper,
		Store: func(ctx context.Context, p autoexplain.ObservedPlan) error {
			return autoexplain.StoreObservedPlan(ctx, pool, p)
		}}
	l := newObservedPlanLoop(sub, sink, rt.spec.Name, logStructuredWrapper)
	rt.start(func() { runObservedPlans(rt.ctx, l, logwatchPollInterval()) })
	rt.note("auto_explain_log")
	return true
}

// observedPlanCap is the per-cycle plan cap: auto_explain.max_plans_per_cycle,
// or its default when unset.
func observedPlanCap(configured int) int {
	if configured <= 0 {
		return config.DefaultAutoExplainMaxPlansPerCycle
	}
	return configured
}

// runObservedPlans ingests the subscription every interval until ctx ends,
// then stops the subscription.
func runObservedPlans(ctx context.Context, l *observedPlanLoop, interval time.Duration) {
	defer l.sub.Stop()
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

// observedPlanLoop is one database's plan ingestion and its totals since
// it started.
type observedPlanLoop struct {
	sub      entryDrainer
	sink     autoexplain.ObservedPlanSink
	database string
	logFn    func(level, format string, args ...any)
	stored   uint64
	evicted  uint64
	rejected map[string]uint64
}

func newObservedPlanLoop(sub entryDrainer, sink autoexplain.ObservedPlanSink, database string,
	logFn func(level, format string, args ...any)) *observedPlanLoop {
	return &observedPlanLoop{sub: sub, sink: sink, database: database, logFn: logFn,
		rejected: make(map[string]uint64)}
}

// tick ingests the entries queued since the last tick and reports what it
// could not keep.
func (l *observedPlanLoop) tick(ctx context.Context) {
	if n := l.sub.TakeEvicted(); n > 0 {
		l.evicted += uint64(n)
		l.logFn("WARN", "db %q: %d log entries were evicted before auto_explain plan "+
			"ingestion read them (subscriber buffer full)", l.database, n)
	}
	entries := l.sub.Drain()
	if len(entries) == 0 {
		return
	}
	res, err := l.sink.Ingest(ctx, entries)
	l.stored += uint64(res.Stored)
	for reason, n := range res.Rejected {
		l.rejected[reason] += uint64(n)
	}
	if err != nil {
		l.logFn("WARN", "db %q: auto_explain plan ingestion: %v", l.database, err)
	}
	if len(res.Rejected) > 0 {
		l.logFn("WARN", "db %q: auto_explain plan records not stored: %s", l.database,
			rejectionSummary(res.Rejected))
	}
}

// rejectionHints name the setting that fixes each actionable reason.
var rejectionHints = map[string]string{
	autoexplain.RejectMissingQueryID: "set auto_explain.log_verbose = on and " +
		"compute_query_id = on",
	autoexplain.RejectInvalidPlan: "set auto_explain.log_format = 'json'",
}

// rejectionSummary lists the reasons in a stable order with their counts
// and, where one exists, the fix.
func rejectionSummary(rejected map[string]int) string {
	reasons := make([]string, 0, len(rejected))
	for reason := range rejected {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		part := reason + "=" + strconv.Itoa(rejected[reason])
		if hint := rejectionHints[reason]; hint != "" {
			part += " (" + hint + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}
