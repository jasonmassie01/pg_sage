package perfgate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderMarkdownListsOffendersInRankOrder(t *testing.T) {
	p := steadyPhase()
	p.Tables = []TableDelta{
		{Name: "sage.decision", LiveRows: 200000, SeqScans: 4, SeqTupRead: 800000},
		{Name: "sage.findings", LiveRows: 200000, SeqScans: 1, SeqTupRead: 200000},
	}
	p.Plans = []PlanResult{{Statement: Statement{QueryID: 3, Query: "SELECT 1"},
		Err: "could not determine data type of parameter $1"}}
	p.Endpoints = []Endpoint{{Path: "/api/v1/findings", Status: 200,
		Duration: 12 * time.Millisecond}}
	offenders, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{p}, offenders)
	for _, want := range []string{
		"# pg_sage performance gate", "small", "## Budgets", "## Offenders (2)",
		"sage.decision", "sage.findings", "## Unexplainable statements (1)",
		"/api/v1/findings",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "sage.decision") > strings.Index(md, "sage.findings") {
		t.Fatalf("decision should be ranked first:\n%s", md)
	}
}

func TestRenderMarkdownClean(t *testing.T) {
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{steadyPhase()}, nil)
	if !strings.Contains(md, "## Offenders (0)") || !strings.Contains(md, "PASS") {
		t.Fatalf("clean report:\n%s", md)
	}
}

func TestRenderMarkdownSuspectsAreNotOffenders(t *testing.T) {
	p := steadyPhase()
	p.Tables = []TableDelta{{Name: "sage.query_store", LiveRows: 200000}}
	p.Plans = []PlanResult{{Statement: Statement{QueryID: 11,
		Query: "SELECT * FROM sage.query_store WHERE captured_at > $1"},
		SeqScans: []PlanScan{{"sage", "query_store"}}}}
	offenders, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil || len(offenders) != 0 {
		t.Fatalf("unconfirmed generic-plan scan became an offender: %+v (%v)", offenders, err)
	}
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{p}, offenders)
	if !strings.Contains(md, "## Generic-plan suspects (1)") ||
		!strings.Contains(md, "queryid 11") {
		t.Fatalf("suspect missing:\n%s", md)
	}
}

func TestRenderMarkdownMeterRow(t *testing.T) {
	p := meteredPhase(100, 140)
	for i := 1; i <= 6; i++ {
		p.Statements = append(p.Statements, Statement{QueryID: int64(100 + i),
			Query: fmt.Sprintf("SELECT untagged_%d", i), Calls: 1, TotalMs: float64(i)})
	}
	store := meteredPhase(10)
	store.Name = "steady (history store)"
	store.Unmetered = "not on the monitored database's meter"
	phases := []Phase{p, store}
	offenders, err := Evaluate(phases, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), phases, offenders)
	for _, want := range []string{
		"| " + string(GateMeterParity) + " | within 10%", string(GateMeterCycle),
		"## Product meter (gate H)", "| steady | 140.0 | 1 of 1 | 103.5 | 15.0 | 15.0 |",
		"untagged_6", "untagged_2", "steady (history store): not charged (not on the " +
			"monitored database's meter)",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
	// The costliest-statements section lists all of them; the meter
	// section only the top 5 untagged.
	meter := md[strings.Index(md, "## Product meter"):]
	meter = meter[:strings.Index(meter, "## Sage table activity")]
	if strings.Contains(meter, "untagged_1 ") || !strings.Contains(meter, "untagged_2 ") {
		t.Fatalf("meter section does not list exactly the top 5 untagged:\n%s", meter)
	}
	if strings.Contains(meter, "/* pg_sage */") {
		t.Fatalf("meter section lists a tagged statement as untagged:\n%s", meter)
	}
	if strings.Index(meter, "untagged_6") > strings.Index(meter, "untagged_5") {
		t.Fatalf("untagged statements not costliest first:\n%s", meter)
	}
}

func TestRenderMarkdownMeterUnknown(t *testing.T) {
	p := meteredPhase(50)
	offenders, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{p}, offenders)
	if !strings.Contains(md, "FAIL") || !strings.Contains(md, "meter unknown") ||
		!strings.Contains(md, "| steady | unknown | 0 of 0 | 50.0 |") {
		t.Fatalf("unknown meter not reported as a failure:\n%s", md)
	}
}
