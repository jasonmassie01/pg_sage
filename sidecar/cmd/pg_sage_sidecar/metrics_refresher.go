package main

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/value"
)

// observability-03: /metrics never touches a database. Each database
// runtime runs one refresher that reads what /metrics exports on its own
// cadence, every source under its own timeout, and publishes the result
// as one immutable snapshot that scrapes render. One unreachable database
// therefore neither stalls a scrape nor blanks the others, and the scrape
// rate no longer drives load on the monitored databases.

// Metrics sources, the source label of the refresh series.
const (
	metricsSourceValue     = "value"
	metricsSourceSLO       = "slo"
	metricsSourceOptimizer = "optimizer"
)

// metricsSourceTimeout bounds every source read of a refresh.
const metricsSourceTimeout = 2 * time.Second

// metricsSnapshot is one refresh of one database. It is never modified
// after it is published. A source that failed has ok false and no values:
// unknown is not exported as stale or zero.
type metricsSnapshot struct {
	value     value.MetricsRead
	slo       []slo.Status
	optimizer map[string]int64
	actions   executor.ActionGauges
	// brief is the owner brief measure (owner_brief_metrics.go).
	brief facts.BriefMeasure
	// sre is the Sage SRE backlog (sre_metrics.go); nil without SRE.
	sre *sreBacklog
	ok  map[string]bool
	// lastSuccess is each source's last successful refresh; it survives
	// failed refreshes so staleness stays measurable.
	lastSuccess map[string]time.Time
}

// reachable reports whether any source read the database.
func (s *metricsSnapshot) reachable() bool {
	for _, ok := range s.ok {
		if ok {
			return true
		}
	}
	return false
}

// metricsSource reads one source into the snapshot being built. It sets
// its snapshot fields only when it returns nil. carry, when set, copies
// the previous snapshot's sample into the new one after a failed read: the
// source exports its last known value (staleness shows in the refresh
// series) instead of none.
type metricsSource struct {
	name  string
	read  func(ctx context.Context, s *metricsSnapshot) error
	carry func(prev, next *metricsSnapshot)
}

// metricsRefresher refreshes one database's metrics snapshot.
type metricsRefresher struct {
	name      string
	sources   []metricsSource
	interval  func() time.Duration
	refreshMu sync.Mutex // one refresh at a time
	snap      atomic.Pointer[metricsSnapshot]
}

func newMetricsRefresher(
	name string, interval func() time.Duration, sources ...metricsSource,
) *metricsRefresher {
	return &metricsRefresher{name: name, sources: sources, interval: interval}
}

// snapshot is the last published refresh; nil before the first one.
func (m *metricsRefresher) snapshot() *metricsSnapshot { return m.snap.Load() }

// run refreshes at once, then every interval, until ctx ends.
func (m *metricsRefresher) run(ctx context.Context) {
	m.refresh(ctx)
	timer := time.NewTimer(m.interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.refresh(ctx)
			timer.Reset(m.interval())
		}
	}
}

// refresh reads every source and publishes the new snapshot. A refresh
// cut short by ctx (shutdown or removal) publishes nothing.
func (m *metricsRefresher) refresh(ctx context.Context) {
	ctx, done := selfbudget.Process().TrackFor(ctx, "metrics_refresh")
	defer done()
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	prev := m.snap.Load()
	next := &metricsSnapshot{ok: map[string]bool{},
		lastSuccess: make(map[string]time.Time, len(m.sources))}
	for _, src := range m.sources {
		err := m.readSource(ctx, src, next)
		if ctx.Err() != nil {
			return
		}
		next.ok[src.name] = err == nil
		if err == nil {
			next.lastSuccess[src.name] = time.Now()
		} else if prev != nil {
			m.keepPrevious(src, prev, next)
		}
		m.logTransition(src.name, prev, err)
	}
	m.snap.Store(next)
}

// keepPrevious carries a failed source's last success and, for a source
// that keeps its sample, its previous values into next.
func (m *metricsRefresher) keepPrevious(src metricsSource, prev, next *metricsSnapshot) {
	if at := prev.lastSuccess[src.name]; !at.IsZero() {
		next.lastSuccess[src.name] = at
	}
	if src.carry != nil {
		src.carry(prev, next)
	}
}

func (m *metricsRefresher) readSource(
	ctx context.Context, src metricsSource, next *metricsSnapshot,
) error {
	srcCtx, cancel := context.WithTimeout(ctx, metricsSourceTimeout)
	defer cancel()
	return src.read(srcCtx, next)
}

// logTransition logs a source's first failure and its recovery, not every
// failed refresh: the refresh series carry the ongoing state.
func (m *metricsRefresher) logTransition(source string, prev *metricsSnapshot, err error) {
	wasOK := prev == nil || prev.ok[source]
	switch {
	case err != nil && wasOK:
		logWarn("metrics", "db %q: refreshing %s metrics failed; its series are "+
			"omitted until a refresh succeeds: %v", m.name, source, err)
	case err == nil && !wasOK:
		logInfo("metrics", "db %q: %s metrics refresh recovered", m.name, source)
	}
}

// metricsRefreshInterval is the refresh cadence: the configured seconds,
// 0 (or negative) meaning the default, never faster than the collector.
func metricsRefreshInterval(configuredSeconds, collectorSeconds int) time.Duration {
	seconds := configuredSeconds
	if seconds <= 0 {
		seconds = config.DefaultPrometheusRefreshSeconds
	}
	if collectorSeconds > seconds {
		seconds = collectorSeconds
	}
	return time.Duration(seconds) * time.Second
}

// metricsRegistry holds the running refreshers by database name.
type metricsRegistry struct {
	mu     sync.Mutex
	byName map[string]*metricsRefresher
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{byName: map[string]*metricsRefresher{}}
}

// processMetrics is the registry /metrics renders.
var processMetrics = newMetricsRegistry()

// register adds m, replacing an earlier runtime of the same database.
func (r *metricsRegistry) register(m *metricsRefresher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[m.name] = m
}

// unregister removes m unless a replacement runtime already took its name.
func (r *metricsRegistry) unregister(m *metricsRefresher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName[m.name] == m {
		delete(r.byName, m.name)
	}
}

// refreshers are the registered refreshers in name order.
func (r *metricsRegistry) refreshers() []*metricsRefresher {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]*metricsRefresher, 0, len(r.byName))
	for _, m := range r.byName {
		out = append(out, m)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// metricsView is one database as a scrape sees it: its sources and the
// snapshot loaded once for the whole scrape (nil before the first refresh).
type metricsView struct {
	name    string
	sources []string
	snap    *metricsSnapshot
}

func (v metricsView) has(source string) bool {
	for _, s := range v.sources {
		if s == source {
			return true
		}
	}
	return false
}

// ok reports whether the source's last refresh succeeded.
func (v metricsView) ok(source string) bool {
	return v.snap != nil && v.snap.ok[source]
}

// views loads every database's snapshot once, in name order.
func (r *metricsRegistry) views() []metricsView {
	refreshers := r.refreshers()
	if refreshers == nil {
		return nil
	}
	out := make([]metricsView, 0, len(refreshers))
	for _, m := range refreshers {
		names := make([]string, 0, len(m.sources))
		for _, src := range m.sources {
			names = append(names, src.name)
		}
		out = append(out, metricsView{name: m.name, sources: names, snap: m.snapshot()})
	}
	return out
}

// startMetricsRefresher is the runtime start hook: it registers this
// database's refresher and runs it as an instance worker, so removing or
// replacing the database stops it and drops its series.
func (rt *databaseRuntime) startMetricsRefresher() {
	m := newRuntimeMetricsRefresher(rt)
	reg := processMetrics
	reg.register(m)
	rt.start(func() {
		defer reg.unregister(m)
		m.run(rt.ctx)
	})
}

// newRuntimeMetricsRefresher reads the value ledger, the action gauges
// (observability-04) and, when the database has one, the SLO engine.
// Standalone also reads the optimizer counts the process-level family
// reports. A database whose Sage SRE investigator
// runs also reads its investigation backlog.
func newRuntimeMetricsRefresher(rt *databaseRuntime) *metricsRefresher {
	sources := []metricsSource{valueMetricsSource(rt.spec.Name, rt.spec.Pool),
		actionMetricsSource(rt.spec.Pool), ownerBriefMetricsSource(rt.spec.Pool)}
	if rt.sloEngine != nil {
		sources = append(sources, sloMetricsSource(rt.sloEngine))
	}
	if rt.spec.Shared {
		sources = append(sources, optimizerMetricsSource(rt.spec.Pool))
	}
	if rt.sre != nil {
		key, _ := rt.sreRuntimeKey()
		sources = append(sources,
			sreMetricsSource(controlBacklogReader(rt.spec.ControlPool, key)))
	}
	runtimeCfg := rt.cfg
	interval := func() time.Duration {
		return metricsRefreshInterval(runtimeCfg.Prometheus.RefreshIntervalSeconds,
			runtimeCfg.Collector.IntervalSeconds)
	}
	return newMetricsRefresher(rt.spec.Name, interval, sources...)
}
