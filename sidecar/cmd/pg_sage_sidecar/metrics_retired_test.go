package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-09, the v2.6.0 rename release: the removed and the old
// names leave every scrape, and the new names carry the same values.

// No concurrent access tests: these render one scrape; the scrape's own
// concurrency is covered by the refresher tests.

// emitsFamily reports whether body has a HELP, TYPE or sample of family.
func emitsFamily(body, family string) bool {
	for _, line := range strings.Split(body, "\n") {
		for _, p := range []string{"# HELP " + family + " ", "# TYPE " + family + " ",
			family + " ", family + "{"} {
			if strings.HasPrefix(line, p) {
				return true
			}
		}
	}
	return false
}

func TestRetiredFamiliesLeaveEveryScrape(t *testing.T) {
	retired := metricsreg.RetiredFamilies()
	if len(retired) == 0 {
		t.Fatal("no retired families: the check is vacuous")
	}
	for _, mode := range []string{"standalone", "fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installContractFleet(t, mode, "zeta", "alpha")
			body := scrapeMetrics(t)
			if !emitsFamily(body, "pg_sage_database_up") {
				t.Fatalf("scrape lacks pg_sage_database_up; the fixture is not wired:\n%s",
					body)
			}
			for _, r := range retired {
				if emitsFamily(body, r.Name) {
					t.Errorf("%s scrape still emits %s (retired in %s):\n%s", mode, r.Name,
						r.Release, grepLines(body, r.Name))
				}
			}
		})
	}
}

// The fleet total keeps its value under the new name, as a gauge.
func TestFleetFindingsOpenReplacesFleetFindingsTotal(t *testing.T) {
	installContractFleet(t, "fleet", "zeta", "alpha", "mid") // findings 0, 1 and 2
	e := lintScrape(t, scrapeMetrics(t))
	if e.kinds["pg_sage_fleet_findings_open"] != "gauge" {
		t.Fatalf("pg_sage_fleet_findings_open TYPE = %q, want gauge",
			e.kinds["pg_sage_fleet_findings_open"])
	}
	m := e.families["pg_sage_fleet_findings_open"].GetMetric()
	if len(m) != 1 || m[0].GetGauge().GetValue() != 3 {
		t.Fatalf("pg_sage_fleet_findings_open = %v, want one sample of 3", m)
	}
}

// The optimizer counts keep their category label under the new name.
func TestOptimizerRecommendationsReplacesTheTotalName(t *testing.T) {
	preserveValueGlobals(t)
	cfg = config.DefaultConfig() // the optimizer gauge reads the process config
	views := []metricsView{{name: "solo", sources: []string{metricsSourceOptimizer},
		snap: &metricsSnapshot{optimizer: map[string]int64{"covering_index": 2,
			"missing_index": 5}, ok: map[string]bool{metricsSourceOptimizer: true}}}}
	var b strings.Builder
	writeOptimizerMetrics(&b, views)
	out := b.String()
	for _, want := range []string{"# TYPE pg_sage_optimizer_recommendations gauge\n",
		"pg_sage_optimizer_recommendations{category=\"covering_index\"} 2\n",
		"pg_sage_optimizer_recommendations{category=\"missing_index\"} 5\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("optimizer metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "_total") {
		t.Errorf("optimizer metrics still use a _total name:\n%s", out)
	}
	// Nil/empty: a failed optimizer refresh renders the header only.
	views[0].snap.ok[metricsSourceOptimizer] = false
	b.Reset()
	writeOptimizerMetrics(&b, views)
	if strings.Contains(b.String(), "pg_sage_optimizer_recommendations{") {
		t.Errorf("failed refresh still renders optimizer samples:\n%s", b.String())
	}
}
