package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/value"
)

// observability-03: /metrics renders refresher snapshots; the refreshers
// read each database on their own cadence with a per-source timeout.

func TestMetricsRefreshInterval(t *testing.T) {
	for _, tc := range []struct {
		configured, collector int
		want                  time.Duration
	}{
		{0, 10, 30 * time.Second},  // 0 means the default
		{0, 60, 60 * time.Second},  // the default is clamped to the collector too
		{5, 60, 60 * time.Second},  // never faster than the collector
		{45, 30, 45 * time.Second}, // slower than the collector is kept
		{60, 60, 60 * time.Second}, // equal is kept
		{-5, 10, 30 * time.Second}, // negative reads as unset
		{30, 0, 30 * time.Second},  // an unset collector interval does not clamp
	} {
		if got := metricsRefreshInterval(tc.configured, tc.collector); got != tc.want {
			t.Errorf("metricsRefreshInterval(%d, %d) = %v, want %v",
				tc.configured, tc.collector, got, tc.want)
		}
	}
}

// Before the first refresh nothing is known: the families keep their
// HELP/TYPE lines and no sample, never an invented 0.
func TestScrapeBeforeFirstRefreshRendersHeadersOnly(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	processMetrics.register(newMetricsRefresher("orders", fixedInterval(time.Hour),
		fakeValueSource("id-orders", 15, 1), fakeSLOSource(0.5)))
	out := scrapeMetrics(t)
	for _, want := range []string{
		"# TYPE pg_sage_value_metrics_up gauge",
		"# TYPE pg_sage_metrics_refresh_success gauge",
		"# TYPE pg_sage_metrics_refresh_timestamp_seconds gauge",
		"# TYPE pg_sage_toil_minutes_saved gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q before the first refresh:\n%s", want, out)
		}
	}
	for _, bad := range []string{
		`pg_sage_value_metrics_up{`, `pg_sage_metrics_refresh_success{`,
		`pg_sage_metrics_refresh_timestamp_seconds{`, `pg_sage_toil_minutes_saved{`,
		`pg_sage_slo_state{`, "pg_sage_connection_up 0",
	} {
		if strings.Contains(out, bad) {
			t.Errorf("invented %q before the first refresh:\n%s", bad, out)
		}
	}
}

// Error propagation / partial failure: a blackholed database neither
// slows the scrape nor hides the healthy database's series.
func TestRefresherHungInstanceDoesNotBlankTheScrape(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	healthy := newMetricsRefresher("healthy", fixedInterval(time.Hour),
		fakeValueSource("id-healthy", 15, 2), fakeSLOSource(0.25))
	hung := newMetricsRefresher("hung", fixedInterval(time.Hour),
		valueMetricsSource("hung", hungPool(t)), sloMetricsSource(blockingSLOReader))
	for _, m := range []*metricsRefresher{healthy, hung} {
		processMetrics.register(m)
		go m.run(ctx)
	}
	assertScrapeUnder(t, time.Second) // while the hung refresh is in flight
	eventually(t, 15*time.Second, "hung refresh to finish", func() bool {
		return hung.snapshot() != nil && healthy.snapshot() != nil
	})
	out := assertScrapeUnder(t, time.Second)
	for _, want := range []string{
		`pg_sage_toil_minutes_saved{database="healthy",feature="analyze_table"} 15`,
		`pg_sage_incidents_avoided_total{database="healthy",kind="xid_wraparound"} 2`,
		`pg_sage_value_metrics_up{database="healthy"} 1`,
		`pg_sage_slo_error_budget_remaining{database="healthy",slo="checkout",kind="app"} 0.25`,
		`pg_sage_metrics_refresh_success{database="healthy",source="value"} 1`,
		`pg_sage_metrics_refresh_success{database="hung",source="value"} 0`,
		`pg_sage_metrics_refresh_success{database="hung",source="slo"} 0`,
		`pg_sage_value_metrics_up{database="hung"} 0`,
		`pg_sage_metrics_refresh_timestamp_seconds{database="healthy",source="value"} `,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{
		`pg_sage_toil_minutes_saved{database="hung"`,
		`pg_sage_slo_state{database="hung"`,
		`pg_sage_metrics_refresh_timestamp_seconds{database="hung"`,
	} {
		if strings.Contains(out, bad) {
			t.Errorf("the hung database exported %q:\n%s", bad, out)
		}
	}
}

func assertScrapeUnder(t *testing.T, limit time.Duration) string {
	t.Helper()
	start := time.Now()
	out := scrapeMetrics(t)
	if elapsed := time.Since(start); elapsed > limit {
		t.Fatalf("scrape took %v, want under %v", elapsed, limit)
	}
	return out
}

// State transitions: ok -> failed -> recovered. A failed source drops its
// values (unknown is not stale), and its timestamp keeps the last success.
func TestRefresherSourceFailureAndRecovery(t *testing.T) {
	withMetricsRegistry(t)
	var fail bool
	src := fakeValueSource("id-a", 15, 1)
	flaky := metricsSource{name: metricsSourceValue,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			if fail {
				return errors.New("permission denied for table action_log")
			}
			return src.read(ctx, s)
		}}
	m := newMetricsRefresher("a", fixedInterval(time.Hour), flaky)
	ctx := context.Background()
	m.refresh(ctx)
	first := m.snapshot()
	if first == nil || !first.ok[metricsSourceValue] || first.lastSuccess[metricsSourceValue].
		IsZero() || first.value.ToilMinutesByFeature["analyze_table"] != 15 {
		t.Fatalf("first refresh snapshot = %+v", first)
	}
	fail = true
	m.refresh(ctx)
	failed := m.snapshot()
	if failed.ok[metricsSourceValue] || failed.value.ToilMinutesByFeature != nil {
		t.Fatalf("failed refresh kept values: %+v", failed)
	}
	if !failed.lastSuccess[metricsSourceValue].Equal(first.lastSuccess[metricsSourceValue]) {
		t.Fatalf("last success = %v, want the first refresh %v",
			failed.lastSuccess[metricsSourceValue], first.lastSuccess[metricsSourceValue])
	}
	time.Sleep(10 * time.Millisecond)
	fail = false
	m.refresh(ctx)
	recovered := m.snapshot()
	if !recovered.ok[metricsSourceValue] ||
		!recovered.lastSuccess[metricsSourceValue].After(first.lastSuccess[metricsSourceValue]) {
		t.Fatalf("recovered snapshot = %+v", recovered)
	}
	if first.ok[metricsSourceValue] != true {
		t.Fatal("a refresh mutated an earlier snapshot")
	}
}

// A refresh cut short by shutdown publishes nothing.
func TestRefresherCancelledRefreshKeepsTheSnapshot(t *testing.T) {
	m := newMetricsRefresher("a", fixedInterval(time.Hour), fakeValueSource("id-a", 1, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.refresh(ctx)
	if m.snapshot() != nil {
		t.Fatalf("cancelled refresh published %+v", m.snapshot())
	}
}

// Concurrency: scrapes during a refresh each see one whole snapshot, never
// the new value of one source next to the old value of another.
func TestRefresherConcurrentScrapesSeeOneSnapshot(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	gen := 0
	gate := make(chan struct{})
	block := false
	m := newMetricsRefresher("orders", fixedInterval(time.Hour),
		metricsSource{name: metricsSourceValue,
			read: func(ctx context.Context, s *metricsSnapshot) error {
				gen++
				return fakeValueSource("id", float64(gen), int64(gen)).read(ctx, s)
			}},
		metricsSource{name: metricsSourceSLO,
			read: func(ctx context.Context, s *metricsSnapshot) error {
				if block {
					<-gate
				}
				return fakeSLOSource(float64(gen)).read(ctx, s)
			}})
	processMetrics.register(m)
	m.refresh(context.Background())
	block = true
	done := make(chan struct{})
	go func() { m.refresh(context.Background()); close(done) }()
	outs := concurrentScrapes(t, 20)
	close(gate)
	<-done
	outs = append(outs, concurrentScrapes(t, 20)...)
	for _, out := range outs {
		toil := metricValue(t, out, `pg_sage_toil_minutes_saved{database="orders",`+
			`feature="analyze_table"}`)
		budget := metricValue(t, out, `pg_sage_slo_error_budget_remaining{`+
			`database="orders",slo="checkout",kind="app"}`)
		if toil != budget {
			t.Fatalf("scrape mixed snapshots: toil %s, budget %s", toil, budget)
		}
	}
	if got := metricValue(t, outs[len(outs)-1], `pg_sage_toil_minutes_saved{`+
		`database="orders",feature="analyze_table"}`); got != "2" {
		t.Fatalf("after the refresh the scrape shows generation %s, want 2", got)
	}
}

func concurrentScrapes(t *testing.T, n int) []string {
	t.Helper()
	outs := make([]string, n)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i] = renderMetricsBody()
		}(i)
	}
	wg.Wait()
	return outs
}

func metricValue(t *testing.T, out, series string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(series) + ` (\S+)\n`)
	match := re.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("series %s missing:\n%s", series, out)
	}
	return match[1]
}

// Two fleet entries for one physical database are exported once, under
// the first name, as /api/v1/value counts them.
func TestScrapeDedupesTheSamePhysicalDatabase(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	for _, name := range []string{"b-twin", "a"} {
		m := newMetricsRefresher(name, fixedInterval(time.Hour),
			fakeValueSource("same-cluster/db", 15, 1))
		processMetrics.register(m)
		m.refresh(context.Background())
	}
	out := scrapeMetrics(t)
	if !strings.Contains(out, `pg_sage_value_metrics_up{database="a"} 1`) ||
		strings.Contains(out, `pg_sage_value_metrics_up{database="b-twin"}`) ||
		strings.Contains(out, `pg_sage_toil_minutes_saved{database="b-twin"`) {
		t.Fatalf("duplicate database not de-duplicated:\n%s", out)
	}
}

func TestMetricsRegistryReplacementAndRemoval(t *testing.T) {
	reg := newMetricsRegistry()
	first := newMetricsRefresher("a", fixedInterval(time.Hour))
	second := newMetricsRefresher("a", fixedInterval(time.Hour))
	reg.register(first)
	reg.register(second) // a replacement runtime for the same database
	reg.unregister(first)
	if got := reg.refreshers(); len(got) != 1 || got[0] != second {
		t.Fatalf("stopping the old runtime removed its replacement: %v", got)
	}
	reg.unregister(second)
	if got := reg.views(); len(got) != 0 {
		t.Fatalf("views after removal = %v", got)
	}
	var nilReg *metricsRegistry
	if nilReg.views() != nil {
		t.Fatal("a nil registry has views")
	}
}

// The run loop refreshes at once, stops with its context and reports its
// work to the self-budget as metrics_refresh.
func TestRefresherRunRefreshesAtOnceAndStops(t *testing.T) {
	before := selfbudget.Process().Snapshot()["metrics_refresh"].Runs
	m := newMetricsRefresher("a", fixedInterval(time.Hour), fakeValueSource("id", 1, 1))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { m.run(ctx); close(stopped) }()
	eventually(t, 5*time.Second, "first refresh", func() bool { return m.snapshot() != nil })
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher did not stop with its context")
	}
	if after := selfbudget.Process().Snapshot()["metrics_refresh"].Runs; after <= before {
		t.Fatalf("metrics_refresh runs = %d, want more than %d", after, before)
	}
}

func TestRefresherRunUsesTheConfiguredCadence(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	m := newMetricsRefresher("a", fixedInterval(20*time.Millisecond),
		metricsSource{name: metricsSourceValue,
			read: func(context.Context, *metricsSnapshot) error {
				mu.Lock()
				runs++
				mu.Unlock()
				return nil
			}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	m.run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if runs < 3 || runs > 20 {
		t.Fatalf("runs in 300ms at a 20ms cadence = %d, want 3-20", runs)
	}
}

func TestConnectionMetricStates(t *testing.T) {
	preserveValueGlobals(t)
	withMetricsRegistry(t)
	installValueFleet(nil)
	if out := renderConnectionMetric(); !strings.Contains(out, "pg_sage_connection_up 0\n") {
		t.Fatalf("fleet with no databases = %q, want 0", out)
	}
	m := newMetricsRefresher("a", fixedInterval(time.Hour), fakeValueSource("id", 1, 1))
	processMetrics.register(m)
	if out := renderConnectionMetric(); strings.Contains(out, "\npg_sage_connection_up ") {
		t.Fatalf("before any refresh = %q, want no sample", out)
	}
	m.refresh(context.Background())
	if out := renderConnectionMetric(); !strings.Contains(out, "pg_sage_connection_up 1\n") {
		t.Fatalf("after a good refresh = %q, want 1", out)
	}
	pool = hungPool(t) // a process pool reports its last health check
	if out := renderConnectionMetric(); strings.Contains(out, "\npg_sage_connection_up ") {
		t.Fatalf("before any health check = %q, want no sample", out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	checkPoolHealth(ctx, pool)
	if out := renderConnectionMetric(); !strings.Contains(out, "pg_sage_connection_up 0\n") {
		t.Fatalf("after a failed health check = %q, want 0", out)
	}
	poolHealth.record(true)
	if out := renderConnectionMetric(); !strings.Contains(out, "pg_sage_connection_up 1\n") {
		t.Fatalf("after a good health check = %q, want 1", out)
	}
}

func renderConnectionMetric() string {
	var b strings.Builder
	writeConnectionMetric(&b, processMetrics.views())
	return b.String()
}

// The runtime start hook registers one refresher per database with the
// sources that database has, and removes it when the runtime stops.
func TestRuntimeMetricsRefresherSources(t *testing.T) {
	withMetricsRegistry(t)
	rtCfg := config.DefaultConfig()
	rtCfg.Collector.IntervalSeconds = 45
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", Pool: hungPool(t)},
		ctx: ctx, cancel: cancel, workers: &sync.WaitGroup{}, cfg: rtCfg}
	rt.startMetricsRefresher()
	got := processMetrics.views()
	if len(got) != 1 || got[0].name != "orders" ||
		fmt.Sprint(got[0].sources) != "[value actions facts]" {
		t.Fatalf("fleet runtime views = %+v, want orders with [value actions facts]", got)
	}
	if d := processMetrics.refreshers()[0].interval(); d != 45*time.Second {
		t.Fatalf("interval = %v, want the 45s collector interval", d)
	}
	cancel()
	rt.workers.Wait()
	if got := processMetrics.views(); len(got) != 0 {
		t.Fatalf("stopped runtime still registered: %+v", got)
	}
	shared := &databaseRuntime{spec: databaseRuntimeSpec{Name: "solo", Pool: hungPool(t),
		Shared: true}, ctx: context.Background(), workers: &sync.WaitGroup{}, cfg: rtCfg}
	m := newRuntimeMetricsRefresher(shared)
	if fmt.Sprint(sourceNames(m.sources)) != "[value actions facts optimizer]" {
		t.Fatalf("standalone sources = %v", sourceNames(m.sources))
	}
}

func sourceNames(sources []metricsSource) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.name)
	}
	return out
}

// Value series still come only from verified ledgers (moved from scrape
// time to refresh time); unknown ledgers stay unexported.
func TestValueMetricsRenderFromSnapshots(t *testing.T) {
	views := []metricsView{
		{name: "a", sources: []string{metricsSourceValue}, snap: &metricsSnapshot{
			ok: map[string]bool{metricsSourceValue: true},
			value: value.MetricsRead{Identity: "x",
				ToilMinutesByFeature: map[string]float64{"b_feat": 2, "a_feat": 1},
				IncidentsByKind:      map[string]int64{"lock_storm": 3}}}},
		{name: "b", sources: []string{metricsSourceValue}, snap: &metricsSnapshot{
			ok: map[string]bool{metricsSourceValue: false}}},
		{name: "c", sources: []string{metricsSourceOptimizer}, snap: &metricsSnapshot{}},
	}
	var b strings.Builder
	writeValueMetrics(&b, views)
	out := b.String()
	wantOrder := []string{
		`pg_sage_toil_minutes_saved{database="a",feature="a_feat"} 1`,
		`pg_sage_toil_minutes_saved{database="a",feature="b_feat"} 2`,
		`pg_sage_incidents_avoided_total{database="a",kind="lock_storm"} 3`,
		`pg_sage_value_metrics_up{database="a"} 1`,
		`pg_sage_value_metrics_up{database="b"} 0`,
	}
	last := -1
	for _, want := range wantOrder {
		at := strings.Index(out, want)
		if at <= last {
			t.Fatalf("want %q after position %d:\n%s", want, last, out)
		}
		last = at
	}
	if strings.Contains(out, `database="c"`) {
		t.Fatalf("a database without a value source got value series:\n%s", out)
	}
}

// Error propagation for the standalone sources: a pool that cannot answer
// marks it failed and the scrape omits its samples instead of reporting a
// zero recommendation count.
func TestStandaloneSourcesFailClosedWithoutADatabase(t *testing.T) {
	preserveValueGlobals(t)
	cfg = config.DefaultConfig() // the optimizer gauge reads the process config
	closed := hungPool(t)
	closed.Close()
	for _, p := range []*metricsRefresher{
		newMetricsRefresher("solo", fixedInterval(time.Hour),
			optimizerMetricsSource(closed)),
		newMetricsRefresher("solo", fixedInterval(time.Hour),
			optimizerMetricsSource(nil)),
	} {
		p.refresh(context.Background())
		snap := p.snapshot()
		if snap == nil || snap.ok[metricsSourceOptimizer] || snap.optimizer != nil {
			t.Fatalf("failed source = %+v, want it failed with no values", snap)
		}
		views := []metricsView{{name: "solo", snap: snap,
			sources: []string{metricsSourceOptimizer}}}
		var b strings.Builder
		writeOptimizerMetrics(&b, views)
		writeMetricsRefreshSeries(&b, views)
		out := b.String()
		if strings.Contains(out, "pg_sage_optimizer_recommendations{") {
			t.Fatalf("failed refresh exported optimizer samples:\n%s", out)
		}
		if !strings.Contains(out, `pg_sage_metrics_refresh_success{database="solo",`+
			`source="optimizer"} 0`) {
			t.Fatalf("failed optimizer refresh not reported:\n%s", out)
		}
	}
	if _, err := queryCounts(context.Background(), nil, optimizerCountsSQL); !errors.Is(err,
		errNoMetricsPool) {
		t.Fatalf("nil pool error = %v, want errNoMetricsPool", err)
	}
}
