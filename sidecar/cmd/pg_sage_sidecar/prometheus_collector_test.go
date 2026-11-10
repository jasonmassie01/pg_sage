package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// collection-08: collector health is exported per database in every mode.

func newMetricsCollector() *collector.Collector {
	return collector.New(nil, config.DefaultConfig(), 170000, func(string, string, ...any) {})
}

func countSeries(out, metric string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, metric+"{") {
			n++
		}
	}
	return n
}

func TestCollectorStatusesFleetHasEveryDatabase(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	for _, name := range []string{"orders", "billing"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Collector: newMetricsCollector(), Status: &fleet.InstanceStatus{}})
	}
	// An instance that failed to start has no collector: nothing to report.
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "broken",
		Status: &fleet.InstanceStatus{Error: "connect: refused"}})
	statuses := collectorStatuses(mgr, nil, "")
	if len(statuses) != 2 || statuses["orders"].BreakerState != collector.BreakerClosed ||
		statuses["billing"].BreakerState != collector.BreakerClosed {
		t.Fatalf("statuses = %+v, want orders and billing, closed", statuses)
	}
	var b strings.Builder
	writeCollectorMetrics(&b, statuses)
	out := b.String()
	for _, want := range []string{
		`pg_sage_collector_breaker_state{database="billing"} 0`,
		`pg_sage_collector_breaker_state{database="orders"} 0`,
		`pg_sage_collector_persist_failures_total{database="orders"} 0`,
		`pg_sage_collector_query_store_failures_total{database="billing"} 0`,
		`pg_sage_collector_breaker_skips_total{database="orders",reason="load"} 0`,
		`pg_sage_collector_breaker_skips_total{database="orders",reason="load_query_error"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %s:\n%s", want, out)
		}
	}
	if n := countSeries(out, "pg_sage_collector_breaker_state"); n != 2 {
		t.Errorf("breaker_state series = %d, want 2 (one per database)", n)
	}
	if strings.Contains(out, `"broken"`) {
		t.Errorf("an instance without a collector was reported:\n%s", out)
	}
	// No cycle has succeeded yet: the timestamp is unknown, not 1970.
	if strings.Contains(out, "pg_sage_collector_last_success_timestamp{") {
		t.Errorf("last success emitted before any success:\n%s", out)
	}
}

func TestCollectorStatusesStandaloneIsNotDoubleCounted(t *testing.T) {
	coll := newMetricsCollector()
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "app", Collector: coll,
		Status: &fleet.InstanceStatus{}})
	if got := collectorStatuses(mgr, coll, "app"); len(got) != 1 {
		t.Errorf("standalone registered in the manager: %d statuses, want 1", len(got))
	}
	// Dedup is by collector, not by name: an instance renamed away from the
	// standalone name must not report the same collector twice.
	got := collectorStatuses(mgr, coll, "postgres")
	if _, ok := got["app"]; !ok || len(got) != 1 {
		t.Errorf("standalone under another instance name = %v, want only app", got)
	}
	other := newMetricsCollector()
	if got := collectorStatuses(mgr, other, "solo"); len(got) != 2 {
		t.Errorf("an unregistered standalone collector = %v, want app and solo", got)
	}
	got = collectorStatuses(nil, coll, "app")
	if _, ok := got["app"]; !ok || len(got) != 1 {
		t.Errorf("standalone without a manager = %v, want app", got)
	}
	if got := collectorStatuses(nil, nil, "app"); len(got) != 0 {
		t.Errorf("no collector at all = %v, want none", got)
	}
}

func TestWriteCollectorMetricsRendersHealth(t *testing.T) {
	success := time.Unix(1_800_000_000, 0)
	statuses := map[string]collector.Status{
		"app": {
			BreakerState: collector.BreakerDormant,
			SkipsTotal: map[string]int64{collector.SkipReasonLoad: 2,
				collector.SkipReasonLoadQueryError: 5},
			LastSuccess:        success,
			Unavailable:        map[string]string{"indexes": "timeout", "io": "denied"},
			PersistFailures:    3,
			QueryStoreFailures: 4,
		},
	}
	var b strings.Builder
	writeCollectorMetrics(&b, statuses)
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_collector_breaker_state gauge",
		"# TYPE pg_sage_collector_breaker_skips_total counter",
		"# TYPE pg_sage_collector_persist_failures_total counter",
		`pg_sage_collector_breaker_state{database="app"} 2`,
		`pg_sage_collector_breaker_skips_total{database="app",reason="load"} 2`,
		`pg_sage_collector_breaker_skips_total{database="app",reason="load_query_error"} 5`,
		`pg_sage_collector_last_success_timestamp{database="app"} 1800000000`,
		`pg_sage_collector_category_unavailable{database="app",category="indexes"} 1`,
		`pg_sage_collector_category_unavailable{database="app",category="io"} 1`,
		`pg_sage_collector_persist_failures_total{database="app"} 3`,
		`pg_sage_collector_query_store_failures_total{database="app"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %s:\n%s", want, out)
		}
	}
	if strings.Index(out, `category="indexes"`) > strings.Index(out, `category="io"`) {
		t.Errorf("unavailable categories are not sorted:\n%s", out)
	}
}

func TestWriteCollectorMetricsEmptyEmitsHeadersOnly(t *testing.T) {
	var b strings.Builder
	writeCollectorMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_collector_breaker_state gauge") ||
		countSeries(out, "pg_sage_collector_breaker_state") != 0 {
		t.Fatalf("empty collector metrics:\n%s", out)
	}
}

func TestMetricsEndpointEmitsCollectorHealthInFleet(t *testing.T) {
	preserveValueGlobals(t)
	oldColl := coll
	t.Cleanup(func() { coll = oldColl })
	installValueFleet(nil)
	coll = nil
	for _, name := range []string{"orders", "billing"} {
		fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name}, Collector: newMetricsCollector(),
			Status: &fleet.InstanceStatus{LastSeen: time.Now()}})
	}
	out := scrapeMetrics(t)
	if n := countSeries(out, "pg_sage_collector_breaker_state"); n != 2 {
		t.Errorf("breaker_state series = %d, want 2:\n%s", n, out)
	}
}
