package main

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// observability-07: the /metrics exposition contract, checked with the
// Prometheus text parser instead of substrings.

// No concurrent access tests: the escaper is pure and the counters' locking
// is covered by their own packages' tests.

// legacyTotalGauges are the gauges that still carry the counter suffix.
// observability-09 renamed or removed the others in v2.6.0, and no new
// gauge may join. pg_sage_incidents_avoided_total is no new family: it
// shipped as a counter and earned-09 made it a gauge (a rollback voids
// credit, and a falling counter reads as a reset) without renaming it.
var legacyTotalGauges = map[string]bool{
	"pg_sage_incidents_avoided_total": true,
}

// contractStart is when the fixture's databases started their first look.
var contractStart = time.Unix(1_700_000_000, 0)

// hostileNames are database names the text format must carry intact.
var hostileNames = []string{"a\tb", `q"x`, "nul\x00z", "bad\xffutf8", `back\slash`,
	"new\nline", "\u2028sep"}

// installContractFleet points the scrape at a manager holding names, with
// the in-memory families that need no database populated.
func installContractFleet(t *testing.T, mode string, names ...string) {
	t.Helper()
	insts := make([]*fleet.DatabaseInstance, 0, len(names))
	for i, n := range names {
		insts = append(insts, healthInstance(n, fleet.InstanceStatus{Connected: i%2 == 0,
			FindingsOpen: i, FindingsCritical: i % 2, HealthScore: 90 - i,
			TrustLevel: "observation", LastSeen: contractStart}))
	}
	installHealthFleet(t, mode, insts...)
	cfg.LLM.TokenBudgetDaily = 1000
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerSharedLLMClient(t) // the LLM families come from the client registry
	fleetLLMBudget = fleet.NewBudget(1000, names)
	oldTracker := firstLookTracker
	t.Cleanup(func() { firstLookTracker = oldTracker })
	firstLookTracker = onboarding.NewTracker()
	for _, n := range names {
		firstLookTracker.Start(n, contractStart)
		firstLookTracker.FirstLook(n, contractStart.Add(2*time.Second), 3, 3,
			time.Second)
	}
}

// exposition is one scrape linted against the text-format contract.
type exposition struct {
	body     string
	families map[string]*dto.MetricFamily
	kinds    map[string]string // family -> TYPE
	order    []string          // sample keys (name and labels), in order
}

// lintScrape parses body and checks the structural contract: every
// family has exactly one HELP and one TYPE line, both before its first
// sample, and the family's samples are contiguous.
func lintScrape(t *testing.T, body string) exposition {
	t.Helper()
	e := exposition{body: body, families: parseExposition(t, body),
		kinds: map[string]string{}}
	help, closed := map[string]bool{}, map[string]bool{}
	current := ""
	for n, line := range strings.Split(body, "\n") {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "# HELP "):
			name := strings.Fields(line)[2]
			if help[name] {
				t.Errorf("line %d: second HELP for %s", n+1, name)
			}
			help[name] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if _, dup := e.kinds[f[2]]; dup {
				t.Errorf("line %d: second TYPE for %s", n+1, f[2])
			}
			e.kinds[f[2]] = f[3]
		default:
			name := summaryFamily(sampleFamily(line), e.kinds)
			if !help[name] || e.kinds[name] == "" {
				t.Errorf("line %d: sample of %s without HELP and TYPE first", n+1, name)
			}
			if name != current {
				if closed[name] {
					t.Errorf("line %d: %s samples are not contiguous", n+1, name)
				}
				closed[current], current = true, name
			}
			e.order = append(e.order, line[:strings.LastIndexByte(line, ' ')])
		}
	}
	for name := range help {
		if e.kinds[name] == "" {
			t.Errorf("%s has HELP but no TYPE", name)
		}
	}
	return e
}

// summaryFamily maps a summary's _sum or _count sample name, or a
// histogram's _bucket, _sum or _count, to the family its HELP and TYPE
// name; any other name is returned unchanged.
func summaryFamily(name string, kinds map[string]string) string {
	for _, suffix := range []string{"_sum", "_count", "_bucket"} {
		base, ok := strings.CutSuffix(name, suffix)
		if !ok {
			continue
		}
		if kinds[base] == "histogram" || (kinds[base] == "summary" && suffix != "_bucket") {
			return base
		}
	}
	return name
}

// sampleFamily is the metric name of a sample line.
func sampleFamily(line string) string {
	if i := strings.IndexAny(line, "{ "); i >= 0 {
		return line[:i]
	}
	return line
}

// labelValues lists every value the label name takes in family.
func (e exposition) labelValues(family, name string) []string {
	var out []string
	for _, m := range e.families[family].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == name {
				out = append(out, l.GetValue())
			}
		}
	}
	return out
}

// Invalid input: hostile database names produce a scrape the parser
// accepts, and each name round-trips. Fails before observability-07: %q
// wrote \t, \x00 and \xff, which the format rejects.
func TestContractHostileDatabaseNamesParse(t *testing.T) {
	for _, mode := range []string{"fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installContractFleet(t, mode, hostileNames...)
			e := lintScrape(t, scrapeMetrics(t))
			for _, family := range []string{"pg_sage_database_up",
				"pg_sage_database_health_score", "pg_sage_llm_fleet_budget_used_tokens",
				"pg_sage_first_look_items"} {
				got := map[string]bool{}
				for _, v := range e.labelValues(family, "database") {
					got[v] = true
				}
				for _, n := range hostileNames {
					if want := strings.ToValidUTF8(n, "\uFFFD"); !got[want] {
						t.Errorf("%s lacks database %q; has %q", family, want,
							e.labelValues(family, "database"))
					}
				}
			}
		})
	}
}

// Happy path / contract: a full scrape parses in every mode, no family
// appears twice, every family has HELP and TYPE, and two scrapes list the
// same series in the same order.
func TestContractScrapeParsesInEveryMode(t *testing.T) {
	for _, mode := range []string{"standalone", "fleet", config.ModeMeta} {
		t.Run(mode, func(t *testing.T) {
			installContractFleet(t, mode, "zeta", "alpha", "mid")
			first := lintScrape(t, scrapeMetrics(t))
			second := lintScrape(t, scrapeMetrics(t))
			if strings.Join(first.order, "\n") != strings.Join(second.order, "\n") {
				t.Fatalf("series order differs between scrapes:\n%s\n---\n%s",
					strings.Join(first.order, "\n"), strings.Join(second.order, "\n"))
			}
			if len(first.order) < 20 {
				t.Fatalf("scrape has only %d samples; the fixture is not wired:\n%s",
					len(first.order), first.body)
			}
			ups := first.labelValues("pg_sage_database_up", "database")
			if strings.Join(ups, ",") != "alpha,mid,zeta" {
				t.Errorf("database_up databases = %q, want alpha,mid,zeta", ups)
			}
		})
	}
}

// Boundary: no gauge outside the pinned legacy set ends in _total, the
// legacy gauge keeps its name and type, and every counter ends in _total.
func TestContractGaugeAndCounterSuffixes(t *testing.T) {
	installContractFleet(t, "fleet", "alpha", "beta")
	e := lintScrape(t, scrapeMetrics(t))
	counters := 0
	for name, kind := range e.kinds {
		switch kind {
		case "gauge":
			if strings.HasSuffix(name, "_total") && !legacyTotalGauges[name] {
				t.Errorf("gauge %s ends in _total, the counter suffix", name)
			}
		case "counter":
			counters++
			if !strings.HasSuffix(name, "_total") {
				t.Errorf("counter %s does not end in _total", name)
			}
		case "summary":
			// sre-06's probe queue wait: only _sum and _count, no quantiles.
			for _, m := range e.families[name].GetMetric() {
				if len(m.GetSummary().GetQuantile()) != 0 {
					t.Errorf("summary %s exports quantiles; want _sum and _count only", name)
				}
			}
		case "histogram":
			// observability-05's LLM request latency: bounded buckets, +Inf last.
			for _, m := range e.families[name].GetMetric() {
				buckets := m.GetHistogram().GetBucket()
				if len(buckets) == 0 || len(buckets) > 8 {
					t.Errorf("histogram %s has %d buckets; want 1-8", name, len(buckets))
				}
			}
		default:
			t.Errorf("family %s has TYPE %s; /metrics exports gauges, counters, "+
				"histograms and quantile-free summaries", name, kind)
		}
	}
	if counters == 0 {
		t.Fatal("no counter family in the scrape; the boundary check is vacuous")
	}
	for name := range legacyTotalGauges {
		if e.kinds[name] != "gauge" {
			t.Errorf("legacy family %s has TYPE %q, want gauge", name, e.kinds[name])
		}
	}
}

// Nil/empty: with every global nil (no pool, no fleet manager, no LLM
// client) the scrape parses and carries the info and build families.
func TestContractNilGlobalsScrapeParses(t *testing.T) {
	installHealthFleet(t, "standalone")
	fleetMgr = nil
	e := lintScrape(t, scrapeMetrics(t))
	info := e.families["pg_sage_info"].GetMetric()
	if len(info) != 1 || info[0].GetGauge().GetValue() != 1 {
		t.Fatalf("pg_sage_info = %v, want one sample of 1\n%s", info, e.body)
	}
	if !strings.Contains(e.body, `pg_sage_build_info{version="dev",commit="none"} 1`+"\n") {
		t.Errorf("scrape lacks pg_sage_build_info{version=\"dev\",commit=\"none\"} 1:\n%s",
			e.body)
	}
	if e.kinds["pg_sage_build_info"] != "gauge" {
		t.Errorf("pg_sage_build_info TYPE = %q, want gauge", e.kinds["pg_sage_build_info"])
	}
	start := e.families["process_start_time_seconds"].GetMetric()
	if len(start) != 1 {
		t.Fatalf("process_start_time_seconds samples = %d, want 1\n%s", len(start), e.body)
	}
	got := start[0].GetGauge().GetValue()
	if want := float64(processStartTime.Unix()); got != want {
		t.Errorf("process_start_time_seconds = %v, want %v", got, want)
	}
	if got <= 0 || got > float64(time.Now().Unix()) {
		t.Errorf("process_start_time_seconds = %v, not a past Unix time", got)
	}
}

// Invalid input: the build and info labels are escaped too; a version
// string from ldflags is free-form.
func TestContractBuildInfoEscapesVersionAndCommit(t *testing.T) {
	installHealthFleet(t, "standalone")
	oldVersion, oldCommit := version, commit
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })
	version, commit = "v2.6.0\t\"rc\"", "abc\\123"
	e := lintScrape(t, scrapeMetrics(t))
	for _, family := range []string{"pg_sage_info", "pg_sage_build_info"} {
		if got := e.labelValues(family, "version"); len(got) != 1 || got[0] != version {
			t.Errorf("%s version = %q, want %q", family, got, version)
		}
	}
	if got := e.labelValues("pg_sage_build_info", "commit"); len(got) != 1 ||
		got[0] != commit {
		t.Errorf("pg_sage_build_info commit = %q, want %q", got, commit)
	}
}

// State transitions: after a fleet reload removes a database, no series
// carries its label; the database that stays keeps its series.
func TestContractFleetReloadRemovalLeavesNoSeries(t *testing.T) {
	installContractFleet(t, "fleet", "alpha", "beta")
	before := lintScrape(t, scrapeMetrics(t))
	if !hasLabelValue(before, "beta") {
		t.Fatal("fixture: beta has no series before the reload")
	}
	reloadRemovingDatabase(t, []string{"alpha", "beta"}, "beta")
	after := lintScrape(t, scrapeMetrics(t))
	if hasLabelValue(after, "beta") {
		t.Errorf("series still carry the removed database:\n%s",
			grepLines(after.body, `"beta"`))
	}
	if got := after.labelValues("pg_sage_database_up", "database"); len(got) != 1 ||
		got[0] != "alpha" {
		t.Errorf("database_up after reload = %q, want alpha only", got)
	}
}

// reloadRemovingDatabase runs the YAML fleet reload owner from names to
// names without removed: prepare, commit and drain, as the controller does.
func reloadRemovingDatabase(t *testing.T, names []string, removed string) {
	t.Helper()
	active, desired := config.DefaultConfig(), config.DefaultConfig()
	for _, n := range names {
		active.Databases = append(active.Databases, config.DatabaseConfig{Name: n})
		if n != removed {
			desired.Databases = append(desired.Databases, config.DatabaseConfig{Name: n})
		}
	}
	cfg.Databases = active.Databases
	owner := &fleetDatabasesOwner{controlName: names[0],
		manager: func() *fleet.DatabaseManager { return fleetMgr }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prepared, err := owner.Prepare(ctx, config.ConfigSnapshot{Config: active},
		config.ConfigSnapshot{Config: desired})
	if err != nil {
		t.Fatalf("prepare reload: %v", err)
	}
	if err := prepared.Commit(ctx); err != nil {
		t.Fatalf("commit reload: %v", err)
	}
	if err := prepared.Drain(ctx); err != nil {
		t.Fatalf("drain reload: %v", err)
	}
	// The fixture's instances have no runtime; a real one's teardown
	// forgets its own first look (database_runtime_firstlook.go).
	firstLookTracker.ForgetRun(removed, contractStart)
	if fleetMgr.GetInstance(removed) != nil {
		t.Fatalf("reload left %s registered", removed)
	}
}

// hasLabelValue reports whether any series has a label equal to v.
func hasLabelValue(e exposition, v string) bool {
	for _, mf := range e.families {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == v {
					return true
				}
			}
		}
	}
	return false
}

func grepLines(body, fragment string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, fragment) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// Boundary: only a summary's own _sum and _count samples map to it; a gauge
// named like one, or another suffix, keeps its own name.
func TestSummaryFamilyMapsOnlySummarySamples(t *testing.T) {
	kinds := map[string]string{"wait_seconds": "summary", "rows": "gauge"}
	for in, want := range map[string]string{
		"wait_seconds_sum":   "wait_seconds",
		"wait_seconds_count": "wait_seconds",
		"wait_seconds_total": "wait_seconds_total",
		"rows_count":         "rows_count",
		"wait_seconds":       "wait_seconds",
		"":                   "",
	} {
		if got := summaryFamily(in, kinds); got != want {
			t.Errorf("summaryFamily(%q) = %q, want %q", in, got, want)
		}
	}
}
