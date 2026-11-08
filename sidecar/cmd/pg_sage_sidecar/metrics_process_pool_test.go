package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-03 review: a database that failed to connect has no
// runtime and so no refresher. Before the refresher, value metrics read
// every registered instance and reported it as up=0; it must still show.
func TestScrapeKeepsAFailedInstanceAsValueDown(t *testing.T) {
	preserveValueGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool = nil
	fleetMgr = fleet.NewManager(cfg)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "down",
		Config: config.DatabaseConfig{Name: "down"},
		Status: &fleet.InstanceStatus{Error: "ping: refused", LastSeen: time.Now()}})
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "a",
		Config: config.DatabaseConfig{Name: "a"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()}})
	healthy := newMetricsRefresher("a", fixedInterval(time.Hour),
		fakeValueSource("cluster/a", 15, 2))
	processMetrics.register(healthy)

	before := scrapeMetrics(t)
	if strings.Contains(before, "pg_sage_connection_up 0") {
		t.Fatalf("a failed instance must not report connection_up 0 before any "+
			"refresh:\n%s", before)
	}
	if !strings.Contains(before, `pg_sage_value_metrics_up{database="down"} 0`) {
		t.Fatalf("failed instance missing before the first refresh:\n%s", before)
	}

	healthy.refresh(context.Background())
	out := scrapeMetrics(t)
	for _, want := range []string{
		"pg_sage_value_metrics_up{database=\"a\"} 1\n" +
			"pg_sage_value_metrics_up{database=\"down\"} 0\n",
		`pg_sage_metrics_refresh_success{database="down",source="value"} 0`,
		`pg_sage_toil_minutes_saved{database="a",feature="analyze_table"} 15`,
		"pg_sage_connection_up 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		`pg_sage_toil_minutes_saved{database="down"`,
		`pg_sage_incidents_avoided_total{database="down"`,
		`pg_sage_metrics_refresh_timestamp_seconds{database="down"`,
		`database="down",source="slo"`,
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("scrape invents %q for a database never read:\n%s", unwanted, out)
		}
	}
}

func TestWithUnrefreshedInstancesNilAndKnown(t *testing.T) {
	views := []metricsView{{name: "b", sources: []string{metricsSourceValue}}}
	if got := withUnrefreshedInstances(views, nil); len(got) != 1 || got[0].name != "b" {
		t.Fatalf("nil manager changed the views: %+v", got)
	}
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "b"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "a"})
	got := withUnrefreshedInstances(views, mgr)
	if len(got) != 2 || got[0].name != "a" || got[1].name != "b" {
		t.Fatalf("views = %+v, want a (placeholder) then b (refresher)", got)
	}
	if got[0].snap == nil || got[0].ok(metricsSourceValue) || got[1].snap != nil {
		t.Fatalf("placeholder must read as a failed value read, refresher untouched: %+v",
			got)
	}
	if len(views) != 1 {
		t.Fatalf("input views were modified: %+v", views)
	}
}

// Only meta mode has a process pool that no runtime refreshes.
func TestStartProcessPoolMetricsOnlyInMetaMode(t *testing.T) {
	withMetricsRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the refresher must not outlive the test
	p := hungPool(t)
	standalone := config.DefaultConfig()
	standalone.Mode = "standalone"
	meta := config.DefaultConfig()
	meta.Mode = config.ModeMeta
	for name, tc := range map[string]struct {
		c *config.Config
		p *pgxpool.Pool
	}{"nil config": {nil, p}, "nil pool": {meta, nil}, "standalone": {standalone, p}} {
		startProcessPoolMetrics(ctx, tc.c, tc.p)
		if processPoolMetrics.Load() != nil {
			t.Fatalf("%s started a process pool refresher", name)
		}
	}
	startProcessPoolMetrics(ctx, meta, p)
	m := processPoolMetrics.Load()
	if m == nil || m.name != processPoolMetricsName || len(m.sources) != 1 ||
		m.sources[0].name != metricsSourcePGStat {
		t.Fatalf("meta mode refresher = %+v, want one pgstat source", m)
	}
	meta.Collector.IntervalSeconds = 10
	if got := m.interval(); got != 30*time.Second {
		t.Fatalf("meta refresher interval = %v, want the 30 s default", got)
	}
	meta.Prometheus.RefreshIntervalSeconds = 90 // read live, like the runtimes'
	if got := m.interval(); got != 90*time.Second {
		t.Fatalf("meta refresher interval = %v, want the configured 90 s", got)
	}
}

// A meta-mode scrape renders the metadata database's pg_stat series from
// the process pool refresher, and omits them while its refresh fails.
func TestMetaModeScrapeRendersProcessPoolPgStat(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	preserveMainWiringGlobals(t)
	preserveValueGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode = config.ModeMeta
	live, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(live.Close)
	pool, coll = live, nil
	fleetMgr = fleet.NewManager(cfg)

	m := newMetricsRefresher(processPoolMetricsName, fixedInterval(time.Hour),
		pgStatMetricsSource(live))
	processPoolMetrics.Store(m)
	if out := scrapeMetrics(t); strings.Contains(out, "pg_sage_connections_total{") {
		t.Fatalf("pg_stat series before the first refresh:\n%s", out)
	}
	m.refresh(context.Background())
	out := scrapeMetrics(t)
	if !strings.Contains(out, `pg_sage_connections_total{state="active"} `) ||
		!strings.Contains(out, "pg_sage_cache_hit_ratio ") {
		t.Fatalf("meta scrape lacks the metadata database's pg_stat series:\n%s", out)
	}
	if strings.Contains(out, `database="metadata"`) {
		t.Fatalf("the metadata database must not appear as a monitored one:\n%s", out)
	}

	down := newMetricsRefresher(processPoolMetricsName, fixedInterval(time.Hour),
		pgStatMetricsSource(hungPool(t)))
	processPoolMetrics.Store(down)
	down.refresh(context.Background())
	if out := scrapeMetrics(t); strings.Contains(out, "pg_sage_connections_total") ||
		strings.Contains(out, "pg_sage_cache_hit_ratio") {
		t.Fatalf("a failed refresh must omit the pg_stat series:\n%s", out)
	}
}
