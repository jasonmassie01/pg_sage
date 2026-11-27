package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// observability-02: per-database health families, rendered from the fleet
// manager's in-memory status in every mode. Listed here, not imported from
// the code under test, so a dropped family fails the test.
var wantHealthFamilies = []string{
	"pg_sage_database_up",
	"pg_sage_collector_last_success_timestamp_seconds",
	"pg_sage_analyzer_last_success_timestamp_seconds",
	"pg_sage_findings_open",
	"pg_sage_database_health_score",
	"pg_sage_emergency_stopped",
	"pg_sage_trust_level",
	"pg_sage_database_size_bytes",
}

// installHealthFleet points the scrape at a fresh manager holding insts.
func installHealthFleet(t *testing.T, mode string, insts ...*fleet.DatabaseInstance) {
	t.Helper()
	preserveMainWiringGlobals(t)
	preserveValueGlobals(t)
	oldLLM, oldMgr := llmClient, llmMgr
	t.Cleanup(func() { llmClient, llmMgr = oldLLM, oldMgr })
	cfg = config.DefaultConfig()
	cfg.Mode = mode
	pool, coll, anal, llmClient, llmMgr, fleetLLMBudget = nil, nil, nil, nil, nil, nil
	fleetMgr = fleet.NewManager(cfg)
	for _, inst := range insts {
		fleetMgr.RegisterInstance(inst)
	}
}

func healthInstance(name string, status fleet.InstanceStatus) *fleet.DatabaseInstance {
	s := status
	return &fleet.DatabaseInstance{Name: name, Config: config.DatabaseConfig{Name: name},
		Status: &s}
}

// sampleLines returns family's sample lines, without HELP/TYPE.
func sampleLines(body, family string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, family+"{") || strings.HasPrefix(line, family+" ") {
			out = append(out, line)
		}
	}
	return out
}

// failOnIOPool is a pool whose every acquire or query is counted; its
// dialer refuses, so a read could never succeed silently either.
func failOnIOPool(t *testing.T) (*pgxpool.Pool, *ioCounter) {
	t.Helper()
	pcfg, err := pgxpool.ParseConfig("postgres://sage@192.0.2.1:5432/meta?sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	counter := &ioCounter{}
	pcfg.ConnConfig.Tracer = counter
	pcfg.ConnConfig.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		counter.n.Add(1)
		return nil, errors.New("the meta pool must not be read by a scrape")
	}
	p, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p, counter
}

// State transitions / modes: every mode exports exactly one database_up
// per registered database, with the right value. Before this item meta
// mode (and fleet, standalone) had no such family at all.
func TestDatabaseHealthModesExportOneUpPerDatabase(t *testing.T) {
	for _, mode := range []string{"standalone", "fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installHealthFleet(t, mode,
				healthInstance("alpha", fleet.InstanceStatus{Connected: true}),
				healthInstance("beta", fleet.InstanceStatus{Connected: false}))
			body := scrapeMetrics(t)
			got := sampleLines(body, "pg_sage_database_up")
			want := []string{`pg_sage_database_up{database="alpha"} 1`,
				`pg_sage_database_up{database="beta"} 0`}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("%s database_up = %q, want %q\n%s", mode, got, want, body)
			}
			for _, family := range wantHealthFamilies {
				if !strings.Contains(body, "# TYPE "+family+" gauge\n") {
					t.Errorf("%s scrape lacks the %s family", mode, family)
				}
			}
		})
	}
}

// Regression: in meta mode pool is the metadata database. No health
// sample may come from it, and it must never appear as a database.
func TestDatabaseHealthMetaModeNeverReadsTheMetaPool(t *testing.T) {
	installHealthFleet(t, config.ModeMeta,
		healthInstance("orders", fleet.InstanceStatus{Connected: true, FindingsCritical: 1,
			FindingsOpen: 1}))
	meta, counter := failOnIOPool(t)
	pool = meta
	body := scrapeMetrics(t)
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("scrape touched the meta pool %d times", n)
	}
	for _, family := range wantHealthFamilies {
		for _, line := range sampleLines(body, family) {
			if !strings.Contains(line, `database="orders"`) {
				t.Errorf("%s sample not labelled with the monitored database: %q",
					family, line)
			}
		}
	}
	if !strings.Contains(body,
		`pg_sage_findings_open{database="orders",severity="critical"} 1`) {
		t.Fatalf("meta scrape lacks the monitored database's findings:\n%s", body)
	}
}

// Happy path: every family carries the status the manager holds. The
// collector sampled just now, so fleet-07's stale cap (25) does not apply.
func TestDatabaseHealthRendersEveryFamily(t *testing.T) {
	collected := time.Now().Truncate(time.Second)
	analyzed := collected.Add(time.Second)
	installHealthFleet(t, "fleet", healthInstance("alpha", fleet.InstanceStatus{
		Connected: true, TrustLevel: "advisory", DatabaseSize: 987654321,
		CollectorLastRun: collected, AnalyzerLastRun: analyzed,
		FindingsOpen: 6, FindingsCritical: 1, FindingsWarning: 2, FindingsInfo: 3,
	}))
	body := scrapeMetrics(t)
	score := 100 - 25 - 2*5 // fleet computeHealthScore: critical 25, warning 5 each
	for _, want := range []string{
		`pg_sage_database_up{database="alpha"} 1` + "\n",
		fmt.Sprintf(`pg_sage_collector_last_success_timestamp_seconds{database="alpha"} %d`,
			collected.Unix()) + "\n",
		fmt.Sprintf(`pg_sage_analyzer_last_success_timestamp_seconds{database="alpha"} %d`,
			analyzed.Unix()) + "\n",
		`pg_sage_findings_open{database="alpha",severity="critical"} 1` + "\n",
		`pg_sage_findings_open{database="alpha",severity="warning"} 2` + "\n",
		`pg_sage_findings_open{database="alpha",severity="info"} 3` + "\n",
		fmt.Sprintf(`pg_sage_database_health_score{database="alpha"} %d`, score) + "\n",
		`pg_sage_emergency_stopped{database="alpha"} 0` + "\n",
		`pg_sage_trust_level{database="alpha",level="observation"} 0` + "\n",
		`pg_sage_trust_level{database="alpha",level="advisory"} 1` + "\n",
		`pg_sage_trust_level{database="alpha",level="autonomous"} 0` + "\n",
		`pg_sage_database_size_bytes{database="alpha"} 987654321` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("scrape:\n%s", body)
	}
}

// Boundary: a zero timestamp or size is unknown, never exported as 0.
func TestDatabaseHealthZeroTimestampsAndSizeAreOmitted(t *testing.T) {
	installHealthFleet(t, "fleet",
		healthInstance("fresh", fleet.InstanceStatus{Connected: true}),
		healthInstance("seen", fleet.InstanceStatus{Connected: true, DatabaseSize: 1,
			CollectorLastRun: time.Unix(1, 0), AnalyzerLastRun: time.Unix(2, 0)}))
	body := scrapeMetrics(t)
	for family, want := range map[string]string{
		"pg_sage_collector_last_success_timestamp_seconds": `{database="seen"} 1`,
		"pg_sage_analyzer_last_success_timestamp_seconds":  `{database="seen"} 2`,
		"pg_sage_database_size_bytes":                      `{database="seen"} 1`,
	} {
		got := sampleLines(body, family)
		if len(got) != 1 || got[0] != family+want {
			t.Errorf("%s = %q, want only %q", family, got, family+want)
		}
	}
}

// Boundary: no trust level yet (a database that never started) has no
// trust samples; an unrecognised level is shown, never hidden.
func TestDatabaseHealthTrustLevelUnknownAndUnrecognised(t *testing.T) {
	installHealthFleet(t, "fleet",
		healthInstance("never", fleet.InstanceStatus{Error: "prepare: refused"}),
		healthInstance("odd", fleet.InstanceStatus{Connected: true, TrustLevel: "bogus"}))
	got := sampleLines(scrapeMetrics(t), "pg_sage_trust_level")
	want := []string{
		`pg_sage_trust_level{database="odd",level="observation"} 0`,
		`pg_sage_trust_level{database="odd",level="advisory"} 0`,
		`pg_sage_trust_level{database="odd",level="autonomous"} 0`,
		`pg_sage_trust_level{database="odd",level="bogus"} 1`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("trust_level = %q, want %q", got, want)
	}
}

// Nil/empty: no manager, or a manager without databases, renders the
// headers only and does not panic.
func TestDatabaseHealthNilManagerAndNoInstances(t *testing.T) {
	empty := fleet.NewManager(config.DefaultConfig())
	for name, mgr := range map[string]*fleet.DatabaseManager{"nil": nil, "empty": empty} {
		var b strings.Builder
		writeDatabaseHealthMetrics(&b, mgr)
		out := b.String()
		for _, family := range wantHealthFamilies {
			if !strings.Contains(out, "# HELP "+family+" ") ||
				!strings.Contains(out, "# TYPE "+family+" gauge\n") {
				t.Errorf("%s manager: %s lacks HELP/TYPE", name, family)
			}
			if got := sampleLines(out, family); len(got) != 0 {
				t.Errorf("%s manager: %s invents samples %q", name, family, got)
			}
		}
	}
}

// Error propagation: an instance whose status carries an error is down,
// and its last known findings stay visible.
func TestDatabaseHealthErroredInstanceIsDownAndKeepsFindings(t *testing.T) {
	installHealthFleet(t, config.ModeMeta, healthInstance("orders", fleet.InstanceStatus{
		Connected: true, Error: "unreachable: dial tcp: refused",
		FindingsOpen: 2, FindingsCritical: 2,
	}))
	body := scrapeMetrics(t)
	for _, want := range []string{
		`pg_sage_database_up{database="orders"} 0` + "\n",
		`pg_sage_findings_open{database="orders",severity="critical"} 2` + "\n",
		`pg_sage_findings_open{database="orders",severity="warning"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q:\n%s", want, body)
		}
	}
}

// State transitions: up follows connectivity, trust follows the ramp and
// emergency_stopped follows the latch, scrape by scrape.
func TestDatabaseHealthFollowsStatusTransitions(t *testing.T) {
	alpha := healthInstance("alpha", fleet.InstanceStatus{Connected: true,
		TrustLevel: "observation"})
	installHealthFleet(t, "fleet", alpha,
		healthInstance("beta", fleet.InstanceStatus{Connected: true}))
	steps := []struct {
		name   string
		mutate func()
		want   []string
	}{
		{"start", func() {}, []string{`pg_sage_database_up{database="alpha"} 1`,
			`pg_sage_trust_level{database="alpha",level="observation"} 1`,
			`pg_sage_emergency_stopped{database="alpha"} 0`}},
		{"down", func() {
			alpha.UpdateStatus(func(s *fleet.InstanceStatus) {
				s.Connected, s.Error = false, "unreachable: refused"
			})
		}, []string{`pg_sage_database_up{database="alpha"} 0`}},
		{"back and advisory", func() {
			alpha.UpdateStatus(func(s *fleet.InstanceStatus) {
				s.Connected, s.Error, s.TrustLevel = true, "", "advisory"
			})
		}, []string{`pg_sage_database_up{database="alpha"} 1`,
			`pg_sage_trust_level{database="alpha",level="observation"} 0`,
			`pg_sage_trust_level{database="alpha",level="advisory"} 1`}},
		{"stopped", func() { fleetMgr.EmergencyStop("alpha") }, []string{
			`pg_sage_emergency_stopped{database="alpha"} 1`,
			`pg_sage_emergency_stopped{database="beta"} 0`}},
	}
	for _, step := range steps {
		step.mutate()
		body := scrapeMetrics(t)
		for _, want := range step.want {
			if !strings.Contains(body, want+"\n") {
				t.Errorf("%s: scrape lacks %q", step.name, want)
			}
		}
	}
}

// Concurrent access: scrapes racing status writers always see one
// database_up per database. (-race is unavailable on this machine.)
func TestDatabaseHealthConcurrentScrapesDuringStatusWrites(t *testing.T) {
	alpha := healthInstance("alpha", fleet.InstanceStatus{Connected: true})
	installHealthFleet(t, "fleet", alpha,
		healthInstance("beta", fleet.InstanceStatus{Connected: true}))
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			alpha.UpdateStatus(func(s *fleet.InstanceStatus) {
				s.Connected, s.FindingsCritical = i%2 == 0, i%5
			})
		}
	}()
	bodies := concurrentScrapes(t, 20)
	close(stop)
	writers.Wait()
	for i, body := range bodies {
		if got := sampleLines(body, "pg_sage_database_up"); len(got) != 2 {
			t.Fatalf("scrape %d database_up = %q, want 2 samples", i, got)
		}
	}
}

// The LLM gauges describe every registered client, so they render in
// every mode once a client exists (they were standalone-only). Since
// observability-05 each sample carries client and database labels.
func TestDatabaseHealthLLMGaugesInEveryMode(t *testing.T) {
	for _, mode := range []string{"standalone", "fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installHealthFleet(t, mode)
			withLLMHealth(t, "http://127.0.0.1:1", 0)
			if got := sampleLines(scrapeMetrics(t), "pg_sage_llm_enabled"); len(got) != 0 {
				t.Fatalf("%s: LLM gauges without a client: %q", mode, got)
			}
			cfg.LLM.Enabled, cfg.LLM.TokenBudgetDaily = false, 777
			llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
			llmClients.add(llmClient, llmRoleGeneral, "", true) // as registerLLMConfigOwner
			body := scrapeMetrics(t)
			for _, want := range []string{
				`pg_sage_llm_enabled{client="general",database=""} 0` + "\n",
				`pg_sage_llm_tokens_budget_daily{client="general",database=""} 777` + "\n"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s scrape lacks %q", mode, want)
				}
			}
		})
	}
}

// The fleet summary renders from the manager in every mode, so meta mode
// (which builds a manager) is no longer blind to it.
func TestDatabaseHealthFleetSummaryInMetaMode(t *testing.T) {
	installHealthFleet(t, config.ModeMeta,
		healthInstance("a", fleet.InstanceStatus{Connected: true}),
		healthInstance("b", fleet.InstanceStatus{Error: "prepare: refused"}))
	body := scrapeMetrics(t)
	for _, want := range []string{"pg_sage_fleet_databases 2\n", "pg_sage_fleet_healthy 1\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("meta scrape lacks %q:\n%s", want, body)
		}
	}
}

// HELP text: connection_up names the control database and points to the
// per-database family.
func TestDatabaseHealthHelpNamesReplacements(t *testing.T) {
	installHealthFleet(t, "standalone",
		healthInstance("alpha", fleet.InstanceStatus{Connected: true}))
	body := scrapeMetrics(t)
	if help := helpLine(t, body, "pg_sage_connection_up"); !strings.Contains(help,
		"control database") || !strings.Contains(help, "pg_sage_database_up") {
		t.Errorf("connection_up HELP = %q, want the control database and "+
			"pg_sage_database_up named", help)
	}
}

func helpLine(t *testing.T, body, family string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# HELP "+family+" ") {
			return line
		}
	}
	return ""
}

// CHECK-09: no mode gate is left in the scrape handler.
func TestPrometheusSourceHasNoModeGates(t *testing.T) {
	src, err := os.ReadFile("prometheus.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	for _, gate := range []string{"IsStandalone()", `Mode == "fleet"`} {
		if strings.Contains(string(src), gate) {
			t.Errorf("prometheus.go still gates metrics on %s", gate)
		}
	}
}

// State transitions: the fleet-wide stop shows on every database,
// including one registered while the latch holds, and a fleet-wide
// resume clears it everywhere.
func TestDatabaseHealthFleetWideStopShowsOnEveryDatabase(t *testing.T) {
	installHealthFleet(t, config.ModeMeta,
		healthInstance("alpha", fleet.InstanceStatus{Connected: true}))
	fleetMgr.EmergencyStop("")
	fleetMgr.RegisterInstance(healthInstance("late", fleet.InstanceStatus{Connected: true}))
	want := []string{`pg_sage_emergency_stopped{database="alpha"} 1`,
		`pg_sage_emergency_stopped{database="late"} 1`}
	if got := sampleLines(scrapeMetrics(t), "pg_sage_emergency_stopped"); strings.Join(got,
		"\n") != strings.Join(want, "\n") {
		t.Fatalf("stopped: emergency_stopped = %q, want %q", got, want)
	}
	fleetMgr.Resume("")
	want = []string{`pg_sage_emergency_stopped{database="alpha"} 0`,
		`pg_sage_emergency_stopped{database="late"} 0`}
	if got := sampleLines(scrapeMetrics(t), "pg_sage_emergency_stopped"); strings.Join(got,
		"\n") != strings.Join(want, "\n") {
		t.Fatalf("resumed: emergency_stopped = %q, want %q", got, want)
	}
}
