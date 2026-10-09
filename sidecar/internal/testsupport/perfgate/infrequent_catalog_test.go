package perfgate

import (
	"strings"
	"testing"
)

// The mean budget is for statements that run every cycle, where a slow
// plan costs every cycle and an index or hint would fix it. A catalog scan
// cached to run at most hourly (the schema guard's structural scan reads
// every column of every table: 92-135 ms once, at 20,000 relations on CI)
// is charged by the catalog max and the cycle's DB time instead.

func catalogScan(calls int64, mean float64) Statement {
	return Statement{QueryID: 9, Calls: calls, MeanMs: mean, MaxMs: mean,
		TotalMs: mean * float64(calls),
		Query: "WITH /* pg_sage */ tables AS (SELECT 1 FROM pg_catalog.pg_attribute att)"}
}

func TestInfrequentCatalogScanIsNotChargedByTheMean(t *testing.T) {
	p := steadyPhase() // 6 cycles
	p.Statements = []Statement{catalogScan(1, 135)}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a once-a-run catalog scan at 135 ms: %+v, want no offender", got)
	}
}

// Boundaries: once per cycle is not infrequent; a non-catalog statement is
// always charged; the catalog max still applies to the infrequent scan.
func TestInfrequentCatalogRuleBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		stmt  Statement
		gates []Gate
	}{
		{"every cycle", catalogScan(6, 135), []Gate{GateStatementMean}},
		{"five of six cycles", catalogScan(5, 135), nil},
		{"over the catalog max", catalogScan(1, 600), []Gate{GateCatalogMax}},
		{"sage table, once", Statement{QueryID: 3, Calls: 1, MeanMs: 135, MaxMs: 135,
			TotalMs: 135, Query: "SELECT /* pg_sage */ 1 FROM sage.findings"},
			[]Gate{GateStatementMean}},
	}
	for _, c := range cases {
		p := steadyPhase()
		p.Statements = []Statement{c.stmt}
		got, err := Evaluate([]Phase{p}, DefaultBudgets())
		if err != nil {
			t.Fatal(err)
		}
		var gates []Gate
		for _, o := range got {
			gates = append(gates, o.Gate)
		}
		if len(gates) != len(c.gates) || (len(gates) > 0 && gates[0] != c.gates[0]) {
			t.Fatalf("%s: gates %v, want %v", c.name, gates, c.gates)
		}
	}
}

// The infrequent scan still counts toward the cycle's DB time.
func TestInfrequentCatalogScanCountsTowardCycleTime(t *testing.T) {
	p := steadyPhase()
	p.Statements = []Statement{catalogScan(1, 450),
		{QueryID: 4, Calls: 6, MeanMs: 50, MaxMs: 60, TotalMs: 17700,
			Query: "SELECT /* pg_sage */ 1 FROM sage.findings"}}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateCycleDBTime {
		t.Fatalf("offenders %+v, want the cycle DB time (18,150 ms over 6 cycles)", got)
	}
}

func TestReportStatesTheInfrequentCatalogRule(t *testing.T) {
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{steadyPhase()}, nil)
	if !strings.Contains(md, "less than once per cycle") {
		t.Fatalf("report does not state the rule:\n%s", md)
	}
}
