package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
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
