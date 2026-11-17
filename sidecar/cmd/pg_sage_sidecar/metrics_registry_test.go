package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/servertls"
	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// observability-09: the scrape and the registry agree. Every family a
// scrape emits is registered with the same type and help and only its
// registered labels, every registered family is emitted in a mode it
// declares, and no writer carries its own help text.

// No concurrent access tests: these compare one rendering with static
// data; Header's concurrent use is tested in internal/metricsreg.

var registryModes = []string{metricsreg.ModeStandalone, metricsreg.ModeFleet,
	metricsreg.ModeMeta}

// fullScrape is a scrape of mode plus the families that render only with
// state the contract fixture has no global for: API TLS, leases, the
// metrics listener, SSO, self cost and budget, the tuning agent, SLOs, the
// Sage SRE backlog and the setup wizard's within-the-hour ratio.
func fullScrape(t *testing.T, mode string) string {
	t.Helper()
	installContractFleet(t, mode, "zeta", "alpha")
	return renderFullScrape(t)
}

// renderFullScrape scrapes the installed contract fleet and appends the
// families fullScrape describes.
func renderFullScrape(t *testing.T) string {
	t.Helper()
	old := metricsListenerState.Load()
	t.Cleanup(func() { metricsListenerState.Store(old) })
	metricsListenerState.Store(listenerUp)
	var b strings.Builder
	scrape := scrapeMetrics(t)
	b.WriteString(scrape)
	var loops map[string]selfbudget.LoopStat // the process tracker may have some
	if !strings.Contains(scrape, "# TYPE pg_sage_self_loop_runs_total ") {
		loops = map[string]selfbudget.LoopStat{"collector:alpha": {Busy: time.Second,
			Runs: 3, Errors: 1, LastSuccess: time.Unix(1_700_000_000, 0)}}
	}
	writeTLSMetrics(&b, registryTLSReloader(t))
	clk := &fixedClock{t: time.Unix(1_700_000_000, 0)}
	writeLeaderMetrics(&b, tickedElector(t, stubLease{held: true}, "meta", clk))
	writeSSODiscoveryMetrics(&b, api.SSODiscoveryStatus{Enabled: true, Ready: true,
		Failures: 2})
	deferred := &deferredIndexGaugeStore{} // config-04c
	deferred.set("alpha", nil, schema.DeferredIndexCounts{Missing: 1})
	writeDeferredIndexMetrics(&b, deferred)
	writeSelfCostMetrics(&b, map[string]selfcost.Cost{"alpha": {Known: true,
		DBTimeKnown: true, DBTimeMsPerCycle: 12, CallsPerCycle: 4, BlocksPerCycle: 9,
		RowsReadPerCycle: 3, RowsWrittenPerCycle: 2, SchemaBytes: 4096}}, 3000)
	writeSelfComponentMetrics(&b, map[string][]selfcost.ComponentCost{"alpha": {
		{Component: "collector", DBTimeMsPerHour: 12}}})
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{"alpha": {CPUKnown: true,
		CPUMsPerCycle: 900, DBKnown: true, DBTimeMsPerHour: 5000, BlocksPerHour: 70_000,
		CycleSeconds: 60, StorageKnown: true, StorageBytes: 2 << 30}},
		selfbudget.CPUReading{Known: true, MsPerCycle: 900},
		selfbudget.Budget{CPUMsPerCycle: 600, DBTimeMsPerHour: 180_000,
			BlocksPerHour: 18_000_000, StorageBytes: 1 << 30}, 100, loops)
	writeOptimizerMemoryMetrics(&b, map[string]analyzer.TuningStats{"alpha": {
		WhatIfSkipped: 1, TokensUsed: 10, TokenLimit: 100}})
	writeSLOStatusMetrics(&b, map[string][]slo.Status{"alpha": {registrySLO()}})
	writeSREInvestigationMetrics(&b, []metricsView{backlogView("alpha", &sreBacklog{
		byState: map[string]int64{"open": 1}, oldestOpen: clk.t})})
	writeSetupMetrics(&b, onboarding.SetupRatio{Installs: 2, Model: 0.5, Channel: 1})
	return b.String()
}

func registryTLSReloader(t *testing.T) *servertls.Reloader {
	t.Helper()
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Options{Serial: 71,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)})
	r, err := servertls.New(pair.CertPath, pair.KeyPath, servertls.Options{})
	if err != nil {
		t.Fatalf("TLS reloader: %v", err)
	}
	return r
}

func registrySLO() slo.Status {
	burn, left := 2.5, 0.4
	return slo.Status{Name: "latency", Database: "alpha", Kind: "latency",
		State: slo.StatePage, BudgetRemaining: &left, Rules: []slo.RuleResult{{
			Long: slo.WindowView{Window: "1h", BurnRate: &burn}}}}
}

// emitted maps each family in body to its TYPE, HELP and sample labels.
type emittedFamily struct {
	kind, help string
	labels     map[string]bool
}

func emittedFamilies(t *testing.T, body string) map[string]*emittedFamily {
	t.Helper()
	e := lintScrape(t, body)
	out := map[string]*emittedFamily{}
	for name, kind := range e.kinds {
		out[name] = &emittedFamily{kind: kind, labels: map[string]bool{}}
	}
	for _, line := range strings.Split(body, "\n") {
		if f, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, help, _ := strings.Cut(f, " ")
			if out[name] != nil {
				out[name].help = help
			}
		}
	}
	for name, fam := range e.families {
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				out[name].labels[l.GetName()] = true
			}
		}
	}
	return out
}

// Contract: a full scrape in each mode emits only registered families,
// each with its registered type, help and labels.
func TestRegistryScrapeEmitsOnlyRegisteredFamilies(t *testing.T) {
	for _, mode := range registryModes {
		t.Run(mode, func(t *testing.T) {
			fams := emittedFamilies(t, fullScrape(t, mode))
			if len(fams) < 80 {
				t.Fatalf("scrape has %d families; the fixture is not wired", len(fams))
			}
			for name, got := range fams {
				checkAgainstRegistry(t, name, got)
			}
		})
	}
}

func checkAgainstRegistry(t *testing.T, name string, got *emittedFamily) {
	t.Helper()
	reg, ok := metricsreg.Lookup(name)
	if !ok {
		t.Errorf("%s is emitted but not registered in internal/metricsreg", name)
		return
	}
	if got.kind != string(reg.Type) || got.help != reg.Help {
		t.Errorf("%s: emitted TYPE %q HELP %q, registered %q %q", name, got.kind,
			got.help, reg.Type, reg.Help)
	}
	allowed := map[string]bool{}
	for _, l := range reg.Labels {
		allowed[l] = true
	}
	for l := range got.labels {
		if !allowed[l] {
			t.Errorf("%s: sample label %q is not registered (registered %v)", name, l,
				reg.Labels)
		}
	}
}

// Contract: every registered family is emitted in at least one mode it
// declares, so the registry lists nothing the sidecar no longer exports.
func TestRegistryEveryFamilyIsEmittedInADeclaredMode(t *testing.T) {
	emittedIn := map[string]map[string]bool{}
	for _, mode := range registryModes {
		for name := range emittedFamilies(t, fullScrape(t, mode)) {
			if emittedIn[name] == nil {
				emittedIn[name] = map[string]bool{}
			}
			emittedIn[name][mode] = true
		}
	}
	var missing []string
	for _, f := range metricsreg.Families() {
		found := false
		for _, m := range f.Modes {
			found = found || emittedIn[f.Name][m]
		}
		if !found {
			missing = append(missing, f.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("registered but never emitted in a declared mode:\n%s",
			strings.Join(missing, "\n"))
	}
}

// Nil/empty: with no fleet manager every family still rendered is
// registered.
func TestRegistryScrapeWithoutAFleetManager(t *testing.T) {
	installHealthFleet(t, config.ModeMeta)
	fleetMgr = nil
	fams := emittedFamilies(t, scrapeMetrics(t))
	if fams["pg_sage_info"] == nil {
		t.Fatal("scrape without a fleet manager lacks pg_sage_info")
	}
	for name, got := range fams {
		checkAgainstRegistry(t, name, got)
	}
}

// Help text lives only in the registry: no writer writes a HELP line.
func TestRegistryWritersCarryNoHelpText(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list package sources: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "# HELP") || strings.Contains(string(src), "# TYPE") {
			t.Errorf("%s writes HELP or TYPE itself; use metricsreg.Header", f)
		}
	}
}
