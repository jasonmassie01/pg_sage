package main

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// observability-01: the boundary rules of the dashboard's panels, and a
// scrape in fleet and meta mode has samples for every family it queries.

// No concurrent access tests: each test renders one scrape serially.

// Boundary: the datasource, the variable filters and a summary's series.
func TestGrafanaRulesBoundaries(t *testing.T) {
	ji := fixtureJobInstance
	cases := map[string]struct{ panel, want string }{
		"panel datasource unset": {`{"id":1,"type":"stat","title":"p1","targets":[` +
			`{"expr":"pg_sage_connection_up{` + strings.ReplaceAll(ji, `"`, `\"`) + `}"}],` +
			`"fieldConfig":{"defaults":{"noValue":"x"}}}`, "datasource is not"},
		"target datasource fixed": {strings.Replace(fixturePanel(1, "stat",
			`pg_sage_connection_up{`+ji+`}`), `"targets":[{`,
			`"targets":[{"datasource":{"uid":"prom-1"},`, 1), "a target's datasource"},
		"database not filtered": {fixturePanel(1, "stat",
			`pg_sage_database_up{`+ji+`}`), `does not filter database=~"$database"`},
		"database equality": {fixturePanel(1, "stat",
			`pg_sage_database_up{`+ji+`,database="alpha"}`), `filter database=~"$database"`},
		"job not filtered": {fixturePanel(1, "stat",
			`pg_sage_connection_up{instance=~"$instance"}`), `filter job=~"$job"`},
		"instance not filtered": {fixturePanel(1, "stat",
			`pg_sage_connection_up{job=~"$job"}`), `filter instance=~"$instance"`},
		"second selector unfiltered": {fixturePanel(1, "timeseries",
			`pg_sage_fleet_healthy{`+ji+`} / `+
				`pg_sage_fleet_databases{job=~"$job"}`),
			"pg_sage_fleet_databases does not filter instance"},
		"no noValue": {strings.Replace(fixturePanel(1, "stat", `pg_sage_connection_up{`+ji+
			`}`), `"no data: feature off"`, `""`, 1), "sets no noValue text"},
		"duplicate id": {fixturePanel(1, "stat", `pg_sage_connection_up{`+ji+`}`) + "," +
			fixturePanel(1, "stat", `pg_sage_fleet_healthy{`+ji+`}`), "share id 1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			wantOneProblem(t, boardProblems(fixtureBoard(t, c.panel)), c.want)
		})
	}
	d := ji + `,database=~"$database"`
	ok := fixturePanel(1, "timeseries", `sum by (limiter) (rate(`+
		`pg_sage_sre_probe_queue_wait_seconds_sum{`+d+`}[5m])) / sum by (limiter) (rate(`+
		`pg_sage_sre_probe_queue_wait_seconds_count{`+d+`}[5m]))`)
	if got := boardProblems(fixtureBoard(t, ok)); len(got) != 0 {
		t.Fatalf("summary _sum and _count with every filter: problems %q", got)
	}
}

// Integration (in-memory fleet): the full contract scrape in standalone,
// fleet and meta mode has at least one sample for every family the board
// queries, and each of them renders in every mode.
func TestGrafanaDashboardFamiliesHaveSamples(t *testing.T) {
	fams := boardFamilyNames(loadGrafanaBoard(t))
	modes := []string{metricsreg.ModeStandalone, metricsreg.ModeFleet, metricsreg.ModeMeta}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			e := lintScrape(t, grafanaScrape(t, mode))
			for _, name := range fams {
				f, _ := metricsreg.Lookup(name)
				if !slices.Contains(f.Modes, mode) {
					t.Errorf("%s does not render in %s mode", name, mode)
					continue
				}
				fam := e.families[name]
				switch {
				case grafanaUnseeded[name] && e.kinds[name] == "":
					t.Errorf("%s mode scrape does not declare %s", mode, name)
				case !grafanaUnseeded[name] && (fam == nil || len(fam.GetMetric()) == 0):
					t.Errorf("%s mode scrape has no %s sample", mode, name)
				}
			}
		})
	}
}

// Integration (real Postgres, meta mode): a monitored database registered
// through the meta API appears in pg_sage_database_up, the $database
// variable's source, and labels every per-database family the board
// queries by its name: registering it adds no other database label (no
// "all", no metadata database). Labels other tests of this package left
// in process-wide counters are read first and allowed.
func TestGrafanaDashboardMetaDatabaseLabels(t *testing.T) {
	targetDSN := testdb.CreateDatabase(t, "grafana_meta_target")
	state, input, userID := metaLifecycleFixture(t)
	// Value globals first: cleanups run last-in first-out, so the meta
	// runtime is drained before cfg, pool and fleetMgr are restored.
	preserveValueGlobals(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	parsed, err := url.Parse(targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	input.DatabaseName = strings.TrimPrefix(parsed.Path, "/")
	before := boardDatabaseLabels(t, lintScrape(t, scrapeMetrics(t)))
	if _, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID); err != nil {
		t.Fatalf("register target: %v", err)
	}
	target := fleetMgr.GetInstance(input.Name)
	if target == nil || target.Pool == nil {
		t.Fatal("target runtime missing")
	}
	cfg.Mode = config.ModeMeta
	pool = state.Pool
	updateInstanceFindings(ctx, target)
	e := lintScrape(t, scrapeMetrics(t))
	if got := e.labelValues("pg_sage_database_up", "database"); len(got) != 1 ||
		got[0] != input.Name {
		t.Fatalf("pg_sage_database_up databases = %q, want [%q]", got, input.Name)
	}
	after := boardDatabaseLabels(t, e)
	for name, dbs := range after {
		for db := range dbs {
			if db != input.Name && (db == "" || db == "all" || !before[name][db]) {
				t.Errorf("%s carries database=%q, want only %q", name, db, input.Name)
			}
		}
	}
	if len(after) < 5 {
		t.Fatalf("only %d per-database board families rendered in meta mode", len(after))
	}
}

// boardDatabaseLabels maps each per-database board family the scrape has
// samples of to its database label values.
func boardDatabaseLabels(t *testing.T, e exposition) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, name := range boardFamilyNames(loadGrafanaBoard(t)) {
		f, _ := metricsreg.Lookup(name)
		if !slices.Contains(f.Labels, "database") || e.families[name] == nil {
			continue
		}
		out[name] = map[string]bool{}
		for _, db := range e.labelValues(name, "database") {
			out[name][db] = true
		}
	}
	return out
}

// grafanaUnseeded are board families the in-memory fixture cannot give a
// sample: they count only through the standing gate's recorded decisions
// or the shadow store, both on a real database. The scrape must still
// declare them; their writers are covered by park_metrics_test.go and
// shadow_runtime_test.go, and prometheus_contract_db_test.go seeds shadow
// decisions on real Postgres.
var grafanaUnseeded = map[string]bool{
	"pg_sage_policy_parks_total":               true,
	"pg_sage_verification_wait_releases_total": true,
	"pg_sage_shadow_decisions_total":           true,
}

// grafanaScrape is the registry's full contract scrape of mode, with the
// state behind the board's per-database panels seeded: collector and
// analyzer runs, database size, the collector breaker, action outcomes and
// gauges, the value ledger and Sage SRE probe queueing.
func grafanaScrape(t *testing.T, mode string) string {
	t.Helper()
	installContractFleet(t, mode, "zeta", "alpha")
	ran := contractStart.Add(time.Minute)
	for _, inst := range fleetMgr.Instances() {
		inst.Status.CollectorLastRun, inst.Status.AnalyzerLastRun = ran, ran
		inst.Status.DatabaseSize = 64 << 20
		inst.Collector = collector.New(nil, cfg, 170000, logStructuredWrapper)
	}
	const db = "alpha"
	t.Cleanup(func() { executor.PruneCounts(db); probes.ForgetQueueStats(db) })
	executor.RecordOutcome(db, "index", "failed")
	executor.RecordVerdict(db, "index", "revert")
	m := newMetricsRefresher(db, fixedInterval(time.Hour), fakeValueSource(db, 15, 2),
		fakeActionsSource(executor.ActionGauges{PendingByRisk: map[string]int64{"high": 1},
			LastFailure: ran}, nil))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	m.refresh(context.Background())
	seedProbeRefusal(t, db)
	return renderFullScrape(t)
}

// seedProbeRefusal makes one probe of db wait for, and be refused, the
// only probe slot while another probe holds it.
func seedProbeRefusal(t *testing.T, db string) {
	t.Helper()
	global := probes.NewLimiter(1)
	holder := probes.NewRunner(nil, probes.Catalog(), global)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = holder.WithSlot(context.Background(), func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	r := probes.NewRunner(nil, probes.Catalog(), global)
	r.SetDatabaseName(db)
	ctx, cancel := context.WithTimeout(probes.WithCaller(context.Background(),
		probes.CallerInvestigation), 20*time.Millisecond)
	err := r.WithSlot(ctx, func(context.Context) error { return nil })
	cancel()
	close(release)
	<-done
	if !errors.Is(err, probes.ErrConcurrencyLimit) {
		t.Fatalf("WithSlot = %v, want a concurrency limit", err)
	}
}
