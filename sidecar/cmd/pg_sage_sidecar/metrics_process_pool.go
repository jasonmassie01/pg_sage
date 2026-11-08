package main

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// processPoolMetrics refreshes the process pool's pg_stat values when no
// database runtime reads them: in meta mode the process pool is the
// metadata database, which pg_sage_connections_total and
// pg_sage_cache_hit_ratio described before observability-03. Nil when a
// runtime reads them (standalone) or there is no process pool (fleet).
var processPoolMetrics atomic.Pointer[metricsRefresher]

// processPoolMetricsName labels the metadata database in refresh logs.
const processPoolMetricsName = "metadata"

// startProcessPoolMetrics starts the process pool's pg_stat refresher in
// meta mode; it stops with ctx.
func startProcessPoolMetrics(ctx context.Context, c *config.Config, p *pgxpool.Pool) {
	if c == nil || p == nil || c.IsStandalone() {
		return
	}
	m := newMetricsRefresher(processPoolMetricsName, func() time.Duration {
		return metricsRefreshInterval(c.Prometheus.RefreshIntervalSeconds,
			c.Collector.IntervalSeconds)
	}, pgStatMetricsSource(p))
	processPoolMetrics.Store(m)
	go m.run(ctx)
}

// pgStatView is the database whose pg_stat values /metrics exports: the
// standalone runtime's refresher, else the process pool's (meta mode).
func pgStatView(views []metricsView) (metricsView, bool) {
	if v, found := sourceView(views, metricsSourcePGStat); found {
		return v, true
	}
	m := processPoolMetrics.Load()
	if m == nil {
		return metricsView{}, false
	}
	return metricsView{name: m.name, sources: []string{metricsSourcePGStat},
		snap: m.snapshot()}, true
}

// withUnrefreshedInstances adds every fleet instance without a refresher
// as a database whose value read failed. Such an instance failed to
// connect or prepare, so no runtime reads it; /metrics keeps reporting it
// as pg_sage_value_metrics_up 0 instead of silently dropping it.
func withUnrefreshedInstances(views []metricsView, mgr *fleet.DatabaseManager) []metricsView {
	if mgr == nil {
		return views
	}
	known := make(map[string]bool, len(views))
	for _, v := range views {
		known[v.name] = true
	}
	out := append([]metricsView(nil), views...)
	for name := range mgr.Instances() {
		if known[name] {
			continue
		}
		out = append(out, metricsView{name: name, sources: []string{metricsSourceValue},
			snap: &metricsSnapshot{ok: map[string]bool{metricsSourceValue: false}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
