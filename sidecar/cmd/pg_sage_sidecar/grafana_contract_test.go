package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-01: the Grafana dashboard queries only families /metrics
// exports. Nine of its panels once queried families no release exported,
// and a scrape of two sidecars or a fleet mixed every series. The checker
// below reads grafana/pg_sage_dashboard.json, extracts every pg_sage_* and
// process_* name from every expr and variable query, and checks it and
// the panel rules against internal/metricsreg.

// No concurrent access tests: a static check over one file.
// No state transition tests: the dashboard has no state.

const (
	grafanaDashboardFile = "../../../grafana/pg_sage_dashboard.json"
	grafanaDatasource    = "${DS_PROMETHEUS}"
)

type grafanaDS struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type grafanaTarget struct {
	Expr         string     `json:"expr"`
	LegendFormat string     `json:"legendFormat"`
	Datasource   *grafanaDS `json:"datasource"`
}

type grafanaFieldConfig struct {
	Defaults struct {
		NoValue string `json:"noValue"`
	} `json:"defaults"`
}

type grafanaPanel struct {
	ID          int                `json:"id"`
	Type        string             `json:"type"`
	Title       string             `json:"title"`
	Datasource  *grafanaDS         `json:"datasource"`
	Targets     []grafanaTarget    `json:"targets"`
	Panels      []grafanaPanel     `json:"panels"`
	FieldConfig grafanaFieldConfig `json:"fieldConfig"`
}

type grafanaVar struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Query      json.RawMessage `json:"query"`
	Datasource *grafanaDS      `json:"datasource"`
}

type grafanaInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	PluginID string `json:"pluginId"`
}

type grafanaBoard struct {
	UID        string         `json:"uid"`
	Version    int            `json:"version"`
	Inputs     []grafanaInput `json:"__inputs"`
	Templating struct {
		List []grafanaVar `json:"list"`
	} `json:"templating"`
	Panels []grafanaPanel `json:"panels"`
}

func parseGrafanaBoard(data []byte) (grafanaBoard, error) {
	var b grafanaBoard
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("parse dashboard JSON: %w", err)
	}
	return b, nil
}

func loadGrafanaBoard(t *testing.T) grafanaBoard {
	t.Helper()
	data, err := os.ReadFile(grafanaDashboardFile)
	if err != nil {
		t.Fatalf("read %s: %v", grafanaDashboardFile, err)
	}
	b, err := parseGrafanaBoard(data)
	if err != nil {
		t.Fatalf("%s: %v", grafanaDashboardFile, err)
	}
	return b
}

// flatPanels lists every panel, the panels of collapsed rows included.
func flatPanels(ps []grafanaPanel) []grafanaPanel {
	var out []grafanaPanel
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatPanels(p.Panels)...)
	}
	return out
}

// promQL is a query variable's expression: Grafana stores it either as a
// string or as an object with a "query" field.
func (v grafanaVar) promQL() string {
	var s string
	if json.Unmarshal(v.Query, &s) == nil {
		return s
	}
	var o struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(v.Query, &o) == nil {
		return o.Query
	}
	return ""
}

// boardQuery is one PromQL expression and where the dashboard has it.
type boardQuery struct {
	where, expr, legend string
	panel               bool // a panel target, which must carry the variable filters
}

func boardQueries(b grafanaBoard) []boardQuery {
	var out []boardQuery
	for _, v := range b.Templating.List {
		if v.Type == "query" {
			out = append(out, boardQuery{where: "variable $" + v.Name, expr: v.promQL()})
		}
	}
	for _, p := range flatPanels(b.Panels) {
		for _, tg := range p.Targets {
			out = append(out, boardQuery{where: fmt.Sprintf("panel %q", p.Title),
				expr: tg.Expr, legend: tg.LegendFormat, panel: true})
		}
	}
	return out
}

var (
	promSelectorRE = regexp.MustCompile(`\b((?:pg_sage|process)_[a-zA-Z0-9_]+)(\{[^}]*\})?`)
	promMatcherRE  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*` +
		`"((?:[^"\\]|\\.)*)"`)
)

// promSel is one series selector: a metric name and its label matchers,
// each as operator plus quoted value (`=~"$database"`).
type promSel struct {
	name     string
	matchers map[string]string
}

func promSelectors(expr string) []promSel {
	var out []promSel
	for _, m := range promSelectorRE.FindAllStringSubmatch(expr, -1) {
		s := promSel{name: m[1], matchers: map[string]string{}}
		for _, mm := range promMatcherRE.FindAllStringSubmatch(m[2], -1) {
			s.matchers[mm[1]] = mm[2] + `"` + mm[3] + `"`
		}
		out = append(out, s)
	}
	return out
}

// seriesSuffixTypes are the series suffixes a family type adds.
var seriesSuffixTypes = map[string][]metricsreg.Type{
	"_sum":    {metricsreg.Summary, metricsreg.Type("histogram")},
	"_count":  {metricsreg.Summary, metricsreg.Type("histogram")},
	"_bucket": {metricsreg.Type("histogram")},
}

// boardFamily resolves a queried series name to its registered family:
// the name itself, or a summary's or histogram's suffixed series.
func boardFamily(name string) (metricsreg.Family, bool) {
	if f, ok := metricsreg.Lookup(name); ok {
		return f, true
	}
	for suffix, types := range seriesSuffixTypes {
		base, cut := strings.CutSuffix(name, suffix)
		if !cut {
			continue
		}
		if f, ok := metricsreg.Lookup(base); ok {
			for _, ty := range types {
				if f.Type == ty {
					return f, true
				}
			}
		}
	}
	return metricsreg.Family{}, false
}

// boardFamilyNames lists the registered families the dashboard queries.
func boardFamilyNames(b grafanaBoard) []string {
	seen := map[string]bool{}
	for _, q := range boardQueries(b) {
		for _, s := range promSelectors(q.expr) {
			if f, ok := boardFamily(s.name); ok {
				seen[f.Name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Contract (happy path): every name the dashboard queries is a registered
// family, and the board passes every panel rule.
func TestGrafanaDashboardMetricsAreRegistered(t *testing.T) {
	b := loadGrafanaBoard(t)
	var unknown []string
	for _, q := range boardQueries(b) {
		for _, s := range promSelectors(q.expr) {
			if _, ok := boardFamily(s.name); !ok {
				unknown = append(unknown, q.where+": "+s.name)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Fatalf("the dashboard queries families /metrics does not export:\n%s",
			strings.Join(unknown, "\n"))
	}
	if n := len(boardFamilyNames(b)); n < 40 {
		t.Fatalf("the dashboard queries %d families; want the sidecar, database, "+
			"executor, LLM, SRE and value rows (40 or more)", n)
	}
}

func TestGrafanaDashboardPassesThePanelRules(t *testing.T) {
	if problems := boardProblems(loadGrafanaBoard(t)); len(problems) > 0 {
		t.Fatalf("dashboard problems:\n%s", strings.Join(problems, "\n"))
	}
}

// grafanaUnexportedFamilies are the nine families the v2.5.0 dashboard
// queried that no release exported.
var grafanaUnexportedFamilies = []string{"pg_sage_circuit_breaker_state",
	"pg_sage_max_connections", "pg_sage_xact_commit_total", "pg_sage_xact_rollback_total",
	"pg_sage_status_n_dead_tup", "pg_sage_blks_read_total", "pg_sage_blks_hit_total",
	"pg_sage_deadlocks_total", "pg_sage_uptime_seconds"}

// Identity: re-importing keeps the uid and the DS_PROMETHEUS input, so
// existing provisioning keeps working, and the version moves on.
func TestGrafanaDashboardKeepsItsIdentity(t *testing.T) {
	data, err := os.ReadFile(grafanaDashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	b := loadGrafanaBoard(t)
	if b.UID != "pg-sage" || b.Version < 2 {
		t.Errorf("uid %q version %d, want uid pg-sage and a version above 1", b.UID,
			b.Version)
	}
	if len(b.Inputs) != 1 || b.Inputs[0].Name != "DS_PROMETHEUS" ||
		b.Inputs[0].PluginID != "prometheus" {
		t.Errorf("__inputs = %+v, want the one DS_PROMETHEUS prometheus input", b.Inputs)
	}
	for _, name := range grafanaUnexportedFamilies {
		if strings.Contains(string(data), name) {
			t.Errorf("the dashboard still names %s", name)
		}
	}
	if !strings.Contains(string(data), "postgres_exporter") {
		t.Error("no panel points to postgres_exporter for the PostgreSQL internals")
	}
}

// Boundary: the variables scope every panel to one sidecar and the chosen
// databases; $database lists the databases pg_sage_database_up reports.
func TestGrafanaDashboardVariables(t *testing.T) {
	vars := map[string]grafanaVar{}
	for _, v := range loadGrafanaBoard(t).Templating.List {
		vars[v.Name] = v
	}
	if v := vars["DS_PROMETHEUS"]; v.Type != "datasource" {
		t.Errorf("DS_PROMETHEUS variable type %q, want datasource", v.Type)
	}
	want := map[string]string{
		"job":      `label_values(pg_sage_info, job)`,
		"instance": `label_values(pg_sage_info{job=~"$job"}, instance)`,
		"database": `label_values(pg_sage_database_up{job=~"$job",instance=~"$instance"}, ` +
			`database)`,
	}
	for name, query := range want {
		v, ok := vars[name]
		if !ok {
			t.Errorf("no $%s variable", name)
			continue
		}
		if v.Type != "query" || v.promQL() != query {
			t.Errorf("$%s: type %q query %q, want query %q", name, v.Type, v.promQL(), query)
		}
		if v.Datasource == nil || v.Datasource.UID != grafanaDatasource {
			t.Errorf("$%s datasource = %+v, want %s", name, v.Datasource, grafanaDatasource)
		}
	}
}
