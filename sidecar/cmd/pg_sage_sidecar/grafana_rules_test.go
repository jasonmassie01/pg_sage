package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// observability-01: the panel rules of the Grafana dashboard, and their
// invalid-input, empty and boundary cases on fixture boards.

// No concurrent access tests: the checker is pure over a parsed board.

const grafanaReadmeFile = "../../../grafana/README.md"

// boardProblems lists every rule the board breaks.
func boardProblems(b grafanaBoard) []string {
	panels := flatPanels(b.Panels)
	if len(panels) == 0 {
		return []string{"the dashboard has no panels"}
	}
	var out []string
	ids := map[int]string{}
	for _, p := range panels {
		if prev, dup := ids[p.ID]; dup {
			out = append(out, fmt.Sprintf("panels %q and %q share id %d", prev, p.Title, p.ID))
		}
		ids[p.ID] = p.Title
		out = append(out, panelProblems(p)...)
	}
	retired := map[string]metricsreg.Retired{}
	for _, r := range metricsreg.RetiredFamilies() {
		retired[r.Name] = r
	}
	for _, q := range boardQueries(b) {
		out = append(out, queryProblems(q, retired)...)
	}
	return out
}

// panelProblems checks one panel: only rows and text panels may have no
// targets, every other panel reads ${DS_PROMETHEUS}, and a query panel
// says what no data means.
func panelProblems(p grafanaPanel) []string {
	where := fmt.Sprintf("panel %q (%s)", p.Title, p.Type)
	if strings.TrimSpace(p.Title) == "" {
		return []string{fmt.Sprintf("panel id %d (%s) has no title", p.ID, p.Type)}
	}
	if p.Type == "row" {
		return nil
	}
	var out []string
	if p.Datasource == nil || p.Datasource.UID != grafanaDatasource {
		out = append(out, where+": datasource is not "+grafanaDatasource)
	}
	if p.Type == "text" {
		if len(p.Targets) > 0 {
			out = append(out, where+": a text panel has targets")
		}
		return out
	}
	if len(p.Targets) == 0 {
		return append(out, where+": has no targets (only row and text panels may)")
	}
	for _, tg := range p.Targets {
		if strings.TrimSpace(tg.Expr) == "" {
			out = append(out, where+": a target has no expr")
		}
		if tg.Datasource != nil && tg.Datasource.UID != grafanaDatasource {
			out = append(out, where+": a target's datasource is not "+grafanaDatasource)
		}
	}
	if strings.TrimSpace(p.FieldConfig.Defaults.NoValue) == "" {
		out = append(out, where+": sets no noValue text, so a feature that is off "+
			"reads as broken")
	}
	return out
}

func queryProblems(q boardQuery, retired map[string]metricsreg.Retired) []string {
	sels := promSelectors(q.expr)
	if len(sels) == 0 {
		return []string{fmt.Sprintf("%s: %q queries no pg_sage family", q.where, q.expr)}
	}
	var out []string
	queried := map[string]bool{"job": true, "instance": true}
	for _, s := range sels {
		f, ok := boardFamily(s.name)
		if r, gone := retired[s.name]; !ok && gone {
			out = append(out, fmt.Sprintf("%s: %s was retired in %s (%s %s)", q.where,
				s.name, r.Release, r.Replacement, r.Note))
			continue
		}
		if !ok {
			out = append(out, fmt.Sprintf("%s: %s is not a registered family", q.where,
				s.name))
			continue
		}
		for _, l := range f.Labels {
			queried[l] = true
		}
		if q.panel {
			out = append(out, selectorProblems(q.where, s, f)...)
		}
	}
	if q.panel && len(out) == 0 {
		out = append(out, groupingProblems(q, queried)...)
	}
	return out
}

var (
	promGroupingRE = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)
	legendLabelRE  = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

// groupingProblems checks that every label a panel target groups by, or
// names in its legend, is one its families carry: a renamed or misspelt
// label would collapse the panel into one unlabelled series.
func groupingProblems(q boardQuery, labels map[string]bool) []string {
	var used []string
	for _, m := range promGroupingRE.FindAllStringSubmatch(q.expr, -1) {
		for _, l := range strings.Split(m[1], ",") {
			used = append(used, strings.TrimSpace(l))
		}
	}
	for _, m := range legendLabelRE.FindAllStringSubmatch(q.legend, -1) {
		used = append(used, m[1])
	}
	var out []string
	for _, l := range used {
		if l != "" && !labels[l] {
			out = append(out, fmt.Sprintf("%s: groups or labels by %q, a label no family "+
				"of %q carries", q.where, l, q.expr))
		}
	}
	return out
}

// selectorProblems checks one panel selector: only registered labels, and
// the $job, $instance and (for a per-database family) $database filters.
func selectorProblems(where string, s promSel, f metricsreg.Family) []string {
	labels := map[string]bool{"job": true, "instance": true}
	for _, l := range f.Labels {
		labels[l] = true
	}
	var out []string
	for l := range s.matchers {
		if !labels[l] {
			out = append(out, fmt.Sprintf("%s: %s has no label %q", where, s.name, l))
		}
	}
	need := []string{"job", "instance"}
	if labels["database"] {
		need = append(need, "database")
	}
	for _, l := range need {
		if want := `=~"$` + l + `"`; s.matchers[l] != want {
			out = append(out, fmt.Sprintf("%s: %s does not filter %s%s", where, s.name, l,
				want))
		}
	}
	return out
}

// boardRowsTable renders the README panel table from the board: one line
// per row with its panels' titles in board order.
func boardRowsTable(b grafanaBoard) []string {
	var out []string
	var row string
	var titles []string
	flush := func() {
		if row != "" {
			out = append(out, "| "+row+" | "+strings.Join(titles, ", ")+" |")
		}
	}
	for _, p := range flatPanels(b.Panels) {
		if p.Type == "row" {
			flush()
			row, titles = p.Title, nil
			continue
		}
		titles = append(titles, p.Title)
	}
	flush()
	return out
}

// The README's panel table is the board's: every row with its panels.
func TestGrafanaReadmeTableMatchesTheBoard(t *testing.T) {
	b := loadGrafanaBoard(t)
	if ps := b.Panels; len(ps) == 0 || ps[0].Type != "row" {
		t.Fatal("the board does not start with a row, so its first panels have no row")
	}
	readme, err := os.ReadFile(grafanaReadmeFile)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(readme), "\r\n", "\n")
	rows := boardRowsTable(b)
	if len(rows) < 6 {
		t.Fatalf("the board has %d rows, want at least 6", len(rows))
	}
	for _, line := range rows {
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Errorf("grafana/README.md lacks the table line\n%s", line)
		}
	}
	_, section, _ := strings.Cut(body, "\n## Dashboard Panels\n")
	section, _, _ = strings.Cut(section, "\n## ")
	if n := strings.Count(section, "\n| ") - 2; n != len(rows) { // header and rule
		t.Errorf("README panel table has %d rows, the board %d", n, len(rows))
	}
}

func fixtureBoard(t *testing.T, panels string) grafanaBoard {
	t.Helper()
	b, err := parseGrafanaBoard([]byte(`{"uid":"pg-sage","version":2,"panels":[` +
		panels + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const fixtureDS = `"datasource":{"type":"prometheus","uid":"${DS_PROMETHEUS}"}`

func fixturePanel(id int, typ, expr string) string {
	return fmt.Sprintf(`{"id":%d,"type":%q,"title":"p%d",%s,"targets":[{"expr":%q}],`+
		`"fieldConfig":{"defaults":{"noValue":"no data: feature off"}}}`, id, typ, id,
		fixtureDS, expr)
}

const fixtureJobInstance = `job=~"$job",instance=~"$instance"`

func wantOneProblem(t *testing.T, problems []string, fragment string) {
	t.Helper()
	if len(problems) != 1 || !strings.Contains(problems[0], fragment) {
		t.Fatalf("problems = %q, want exactly one naming %q", problems, fragment)
	}
}

// Invalid input: an unknown family, a retired one, a gauge read as a
// summary series, a misspelt label and broken JSON.
func TestGrafanaRulesRejectInvalidQueries(t *testing.T) {
	cases := map[string]struct{ expr, want string }{
		"unknown family": {`pg_sage_nope{` + fixtureJobInstance + `}`,
			"pg_sage_nope is not a registered family"},
		"retired family": {`pg_sage_cache_hit_ratio{` + fixtureJobInstance + `}`,
			"pg_sage_cache_hit_ratio was retired in v2.6.0"},
		"gauge _count": {`pg_sage_connection_up_count{` + fixtureJobInstance + `}`,
			"pg_sage_connection_up_count is not a registered family"},
		"bad label": {`pg_sage_connection_up{breaker="llm",` + fixtureJobInstance + `}`,
			`pg_sage_connection_up has no label "breaker"`},
		"no family": {`vector(1)`, "queries no pg_sage family"},
		"bad grouping label": {`sum by (outcom) (pg_sage_actions_total{` + fixtureJobInstance +
			`,database=~"$database"})`, `groups or labels by "outcom"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := fixtureBoard(t, fixturePanel(1, "stat", c.expr))
			wantOneProblem(t, boardProblems(b), c.want)
		})
	}
	legend := strings.Replace(fixturePanel(1, "stat", `pg_sage_connection_up{`+
		fixtureJobInstance+`}`), `"targets":[{`, `"targets":[{"legendFormat":"{{database}}",`, 1)
	wantOneProblem(t, boardProblems(fixtureBoard(t, legend)), `labels by "database"`)
	if _, err := parseGrafanaBoard([]byte(`{"panels":[`)); err == nil ||
		!strings.Contains(err.Error(), "parse dashboard JSON") {
		t.Fatalf("broken JSON error = %v", err)
	}
}

// Nil/empty: only rows and text panels may have no targets; a board with
// no panels, or a null panel list, is a problem.
func TestGrafanaRulesEmptyPanels(t *testing.T) {
	text := `{"id":2,"type":"text","title":"t",` + fixtureDS + `}`
	row := `{"id":1,"type":"row","title":"r","panels":[]}`
	if got := boardProblems(fixtureBoard(t, row+","+text+","+
		fixturePanel(3, "stat", `pg_sage_connection_up{`+fixtureJobInstance+`}`))); len(got) != 0 {
		t.Fatalf("row and text without targets: problems %q", got)
	}
	empty := `{"id":4,"type":"stat","title":"s",` + fixtureDS + `,"targets":[]}`
	wantOneProblem(t, boardProblems(fixtureBoard(t, empty)), `"s" (stat): has no targets`)
	noExpr := strings.Replace(fixturePanel(5, "stat", ""), `"expr":""`, `"expr":"  "`, 1)
	got := boardProblems(fixtureBoard(t, noExpr))
	if len(got) != 2 || !strings.Contains(got[0], "a target has no expr") {
		t.Fatalf("blank expr: problems %q", got)
	}
	wantOneProblem(t, boardProblems(fixtureBoard(t, "")), "the dashboard has no panels")
	b, err := parseGrafanaBoard([]byte(`{"panels":null}`))
	if err != nil {
		t.Fatal(err)
	}
	wantOneProblem(t, boardProblems(b), "the dashboard has no panels")
	untitled := `{"id":6,"type":"row","title":""}`
	wantOneProblem(t, boardProblems(fixtureBoard(t, untitled)), "id 6 (row) has no title")
}
