package perfgate

import (
	"strings"
	"testing"
)

// The cluster's database size is a stat of every file of every database:
// its time grows with the cluster's file count and disk, and no index,
// hint or setting changes it (124 ms on CI against 37 ms locally on the
// same fixture). Gate B's mean budget does not charge it, by its tag and
// with the reason; gate D (catalog max) and the cycle's DB time still do.

func TestMeanGateExemptsOnlyTaggedSizingStatements(t *testing.T) {
	b := DefaultBudgets()
	reason, ok := b.MeanExempt["sre:cluster_database_size"]
	if !ok || !strings.Contains(reason, "file") {
		t.Fatalf("cluster size exemption = %q %t, want the per-file reason", reason, ok)
	}
	for tag, why := range b.MeanExempt {
		if !strings.Contains(tag, ":") || len(why) < 40 {
			t.Fatalf("exemption %q = %q: a statement tag with a reason, please", tag, why)
		}
	}
	p := steadyPhase()
	p.Statements = []Statement{
		{QueryID: 1, Query: "SELECT /* pg_sage sre:cluster_database_size v1 */ " +
			"sum(pg_catalog.pg_database_size(d.oid)) FROM pg_catalog.pg_database d",
			Calls: 3, TotalMs: 371, MeanMs: 124, MaxMs: 145},
		{QueryID: 2, Query: "SELECT /* pg_sage */ 1 FROM sage.findings",
			Calls: 3, TotalMs: 371, MeanMs: 124, MaxMs: 145},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateStatementMean ||
		!strings.Contains(got[0].Subject, "sage.findings") {
		t.Fatalf("offenders = %+v, want only the findings statement", got)
	}
}

// The exempt statement still counts toward the cycle's DB time and the
// catalog max.
func TestMeanExemptStatementStillChargedElsewhere(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Statements = []Statement{{QueryID: 1,
		Query: "SELECT /* pg_sage sre:cluster_database_size v1 */ " +
			"sum(pg_catalog.pg_database_size(d.oid)) FROM pg_catalog.pg_database d",
		Calls: 6, TotalMs: 20000, MeanMs: 3333, MaxMs: 4000}}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatal(err)
	}
	gates := map[Gate]bool{}
	for _, o := range got {
		gates[o.Gate] = true
	}
	if gates[GateStatementMean] || !gates[GateCycleDBTime] || !gates[GateCatalogMax] {
		t.Fatalf("gates = %v, want cycle DB time and catalog max, not the mean", gates)
	}
}

func TestReportListsMeanExemptions(t *testing.T) {
	b := DefaultBudgets()
	md := RenderMarkdown(Scale{}, b, []Phase{steadyPhase()}, nil)
	if !strings.Contains(md, "sre:cluster_database_size") ||
		!strings.Contains(md, b.MeanExempt["sre:cluster_database_size"]) {
		t.Fatalf("report omits the mean exemption:\n%s", md)
	}
}
