package perfgate

import (
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
