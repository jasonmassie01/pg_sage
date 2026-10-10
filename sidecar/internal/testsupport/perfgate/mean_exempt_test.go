package perfgate

import (
	"fmt"
	"strings"
	"testing"
)

// Gate B's mean exempts a statement only by the tag in its comment, and
// only up to that tag's own ceiling: the exemption says why the statement
// cannot meet the 100 ms budget and how slow it may be, not that it may be
// as slow as it likes. Above its ceiling it is a gate B offender. Gate D
// (catalog max) and the cycle's DB time still charge it in full.

const (
	clusterSizeTag       = "sre:cluster_database_size"
	structuralColumnsTag = "structural:columns"
)

func clusterSize(mean float64) Statement {
	return Statement{QueryID: 1, Calls: 6, MeanMs: mean, MaxMs: mean, TotalMs: 6 * mean,
		Query: "/* pg_sage sre:cluster_database_size v1 */ SELECT " +
			"sum(pg_catalog.pg_database_size(d.oid)) FROM pg_catalog.pg_database d"}
}

// structuralColumns is the schema guard's column summary as
// pg_stat_statements keeps it (the tag after the first keyword): one call
// per structural pass, every user table on a full pass.
func structuralColumns(mean float64) Statement {
	return Statement{QueryID: 2, Calls: 1, MeanMs: mean, MaxMs: mean, TotalMs: mean,
		Query: "SELECT /* pg_sage structural:columns */ att.attrelid, count(*) FILTER " +
			"(WHERE NOT att.attisdropped) FROM pg_catalog.pg_attribute att " +
			"WHERE att.attrelid = ANY($1::oid[]) AND att.attnum>$3 GROUP BY att.attrelid"}
}

func meanGates(t *testing.T, b Budgets, stmts ...Statement) []Offender {
	t.Helper()
	p := steadyPhase()
	p.Statements = stmts
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var out []Offender
	for _, o := range got {
		if o.Gate == GateStatementMean {
			out = append(out, o)
		}
	}
	return out
}

func TestDefaultMeanExemptionsAreNamedAndCapped(t *testing.T) {
	b := DefaultBudgets()
	if len(b.MeanExempt) != 2 {
		t.Fatalf("mean exemptions = %v, want cluster size and the structural column summary",
			b.MeanExempt)
	}
	size, ok := b.MeanExempt[clusterSizeTag]
	if !ok || size.CeilingMs != 300 || !strings.Contains(size.Reason, "file") ||
		!strings.Contains(size.Reason, "105-250 ms") {
		t.Fatalf("cluster size exemption = %+v %t, want a 300 ms ceiling and the "+
			"per-file reason with the CI range", size, ok)
	}
	// The whole-catalog scan is gone: its column summary is the statement
	// that reads every column of every user table, on a full pass only.
	cols, ok := b.MeanExempt[structuralColumnsTag]
	if !ok || cols.CeilingMs != 150 || !strings.Contains(cols.Reason, "full pass") ||
		!strings.Contains(cols.Reason, "steady phase") ||
		!strings.Contains(cols.Reason, "92-135 ms") {
		t.Fatalf("structural column summary exemption = %+v %t, want a 150 ms ceiling, "+
			"when its full pass runs and the CI range of the scan it replaced", cols, ok)
	}
	if retired, ok := b.MeanExempt["schema_guard:structural"]; ok {
		t.Fatalf("the retired whole-catalog scan's tag is still exempt: %+v", retired)
	}
	for tag, ex := range b.MeanExempt {
		if !strings.Contains(tag, ":") || len(ex.Reason) < 40 ||
			ex.CeilingMs <= b.StatementMeanMs {
			t.Fatalf("exemption %q = %+v: a statement tag, a reason and a ceiling "+
				"above the %v ms budget, please", tag, ex, b.StatementMeanMs)
		}
	}
}

// Boundaries: at the ceiling is within it; just over is an offender judged
// against the ceiling, and its detail names the exemption.
func TestMeanExemptionCeilingBoundaries(t *testing.T) {
	b := DefaultBudgets()
	cases := []struct {
		name    string
		stmt    Statement
		charged bool
	}{
		{"cluster size at 124 ms", clusterSize(124), false},
		{"cluster size exactly at its ceiling", clusterSize(300), false},
		{"cluster size just over its ceiling", clusterSize(300.01), true},
		{"column summary as slow as the scan it replaced", structuralColumns(135), false},
		{"column summary exactly at its ceiling", structuralColumns(150), false},
		{"column summary just over its ceiling", structuralColumns(150.01), true},
	}
	for _, c := range cases {
		got := meanGates(t, b, c.stmt)
		if !c.charged {
			if len(got) != 0 {
				t.Fatalf("%s: %+v, want no gate B offender", c.name, got)
			}
			continue
		}
		ceiling := b.MeanExempt[clusterSizeTag].CeilingMs
		tag := clusterSizeTag
		if c.stmt.QueryID == 2 {
			ceiling, tag = b.MeanExempt[structuralColumnsTag].CeilingMs, structuralColumnsTag
		}
		if len(got) != 1 || got[0].Budget != ceiling || got[0].Measured != c.stmt.MeanMs ||
			!strings.Contains(got[0].Detail, tag) {
			t.Fatalf("%s: %+v, want one gate B offender against the %v ms ceiling of %s",
				c.name, got, ceiling, tag)
		}
	}
}

// Negative cases: an untagged statement is judged by the 100 ms budget
// (at it passes, over it fails); a near-miss tag or the tag outside a
// pg_sage comment exempts nothing.
func TestMeanExemptionNeedsTheExactTag(t *testing.T) {
	b := DefaultBudgets()
	untagged := func(q string, mean float64) Statement {
		return Statement{QueryID: 7, Calls: 6, MeanMs: mean, MaxMs: mean, TotalMs: 6 * mean,
			Query: q}
	}
	cases := []struct {
		name    string
		stmt    Statement
		charged bool
	}{
		{"untagged at the budget", untagged("/* pg_sage */ SELECT 1 FROM sage.findings",
			100), false},
		{"untagged just over the budget", untagged("/* pg_sage */ SELECT 1 FROM "+
			"sage.findings", 100.01), true},
		{"untagged sizing at 124 ms", untagged("/* pg_sage */ SELECT "+
			"sum(pg_catalog.pg_database_size(d.oid)) FROM pg_catalog.pg_database d", 124), true},
		{"near-miss tag", untagged("/* pg_sage sre:cluster_database_sizes v1 */ SELECT 1 "+
			"FROM pg_catalog.pg_database d", 124), true},
		{"tag outside a pg_sage comment", untagged("SELECT 'sre:cluster_database_size' "+
			"FROM pg_catalog.pg_database d", 124), true},
	}
	for _, c := range cases {
		got := meanGates(t, b, c.stmt)
		if c.charged != (len(got) == 1) || len(got) > 1 {
			t.Fatalf("%s: %+v, charged want %t", c.name, got, c.charged)
		}
		if c.charged && got[0].Budget != b.StatementMeanMs {
			t.Fatalf("%s: judged against %v, want the %v ms budget", c.name, got[0].Budget,
				b.StatementMeanMs)
		}
	}
}

// Only the structural pass's column summary is exempt: its table listing,
// text types and column versions are judged by the 100 ms budget, and so
// is a statement still carrying the retired whole-catalog scan's tag.
func TestOnlyTheStructuralColumnSummaryIsExempt(t *testing.T) {
	b := DefaultBudgets()
	for _, q := range []string{
		"SELECT /* pg_sage structural:tables */ tbl.oid FROM pg_catalog.pg_class tbl",
		"SELECT /* pg_sage structural:text_types */ oid FROM pg_catalog.pg_type",
		"SELECT /* pg_sage structural:versions */ att.attrelid " +
			"FROM pg_catalog.pg_attribute att GROUP BY att.attrelid",
		"/* pg_sage schema_guard:structural v1 */\nWITH tables AS (SELECT 1 " +
			"FROM pg_class tbl JOIN pg_attribute att ON att.attrelid = tbl.oid)",
	} {
		s := Statement{QueryID: 3, Calls: 1, MeanMs: 120, MaxMs: 120, TotalMs: 120, Query: q}
		got := meanGates(t, b, s)
		if len(got) != 1 || got[0].Budget != b.StatementMeanMs ||
			strings.Contains(got[0].Detail, "exempt tag") {
			t.Fatalf("120 ms %q: %+v, want one gate B offender against the %v ms budget",
				q, got, b.StatementMeanMs)
		}
	}
}

// The exempt statement still counts toward the cycle's DB time and the
// catalog max.
func TestMeanExemptStatementStillChargedElsewhere(t *testing.T) {
	p := steadyPhase()
	s := clusterSize(250)
	s.TotalMs, s.MaxMs = 20000, 600
	p.Statements = []Statement{s}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
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

// Invalid input: an exemption without a reason, or with a ceiling that is
// missing or not above the budget, is a broken budget.
func TestMeanExemptionValidation(t *testing.T) {
	for name, ex := range map[string]MeanExemption{
		"no ceiling":        {Reason: strings.Repeat("why ", 12)},
		"negative ceiling":  {Reason: strings.Repeat("why ", 12), CeilingMs: -1},
		"ceiling at budget": {Reason: strings.Repeat("why ", 12), CeilingMs: 100},
		"no reason":         {CeilingMs: 300},
		"whitespace reason": {Reason: "   ", CeilingMs: 300},
	} {
		b := DefaultBudgets()
		b.MeanExempt = map[string]MeanExemption{"x:y": ex}
		_, err := Evaluate([]Phase{steadyPhase()}, b)
		if err == nil || !strings.Contains(err.Error(), "x:y") {
			t.Fatalf("%s: err = %v, want the exemption named", name, err)
		}
	}
	b := DefaultBudgets()
	b.MeanExempt = map[string]MeanExemption{"": {Reason: strings.Repeat("why ", 12),
		CeilingMs: 300}}
	if _, err := Evaluate([]Phase{steadyPhase()}, b); err == nil {
		t.Fatal("an empty tag was accepted")
	}
	b.MeanExempt = nil
	if got := meanGates(t, b, clusterSize(124)); len(got) != 1 {
		t.Fatalf("without exemptions the cluster size is charged: %+v", got)
	}
}

func TestReportListsMeanExemptionsWithCeilings(t *testing.T) {
	b := DefaultBudgets()
	md := RenderMarkdown(Scale{}, b, []Phase{steadyPhase()}, nil)
	for tag, ex := range b.MeanExempt {
		line := fmt.Sprintf("- %s (ceiling %.0f ms mean): %s", tag, ex.CeilingMs, ex.Reason)
		if !strings.Contains(md, line) {
			t.Fatalf("report omits %q:\n%s", line, md)
		}
	}
}
