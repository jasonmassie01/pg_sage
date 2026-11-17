package planstability

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/extstats"
)

func addresses() TableFacts {
	return TableFacts{Status: StatusOK, RelTuples: 500_000, Owned: true,
		Columns: map[string]ColumnFacts{"city": col(0, 900), "zip": col(0, 950),
			"id": col(0, -1)}}
}

func addrQuery() query {
	return query{qid: "202", rels: []string{"public.addresses"}, execMs: 50_000, calls: 100,
		rows: 500_000, text: "SELECT id FROM public.addresses WHERE city = $1 AND zip = $2"}
}

// scanPlan is a plan whose scan of addresses estimates est rows; act < 0
// leaves out the actual rows (no auto_explain).
func scanPlan(est, act float64) workpack.PlanFact {
	node := map[string]any{"Node Type": "Seq Scan", "Relation Name": "addresses",
		"Schema": "public", "Plan Rows": est}
	if act >= 0 {
		node["Actual Rows"], node["Actual Loops"] = act, 1.0
	}
	return workpack.PlanFact{ID: "plan/query/202", QueryID: "202", Plan: "ok",
		Source: "auto_explain", Conditions: []any{map[string]any{"Plan": map[string]any{
			"Node Type": "Gather", "Plan Rows": est, "Plans": []any{node}}}}}
}

func addrStats() map[string][]string {
	return map[string][]string{"public.addresses": {"city", "zip"}}
}

func TestGenerate_CorrelatedPredicatesGiveExtendedStatistics(t *testing.T) {
	src := source(map[string]TableFacts{"public.addresses": addresses()})
	p := packOf(t, []query{addrQuery()}, addrStats(), []workpack.PlanFact{scanPlan(10, 5000)})
	cands, _ := generate(t, src, p)
	stx := byAction(cands, "create_statistics")
	if len(stx) != 1 {
		t.Fatalf("create_statistics candidates = %+v, want 1", cands)
	}
	c := stx[0]
	parsed, err := extstats.ParseCreate(c.ForwardSQL)
	if err != nil {
		t.Fatalf("forward %q is not the extstats form: %v", c.ForwardSQL, err)
	}
	if !reflect.DeepEqual(parsed.Kinds, []string{"dependencies", "mcv"}) ||
		!reflect.DeepEqual(parsed.Columns, []string{"city", "zip"}) ||
		parsed.QualifiedTable() != "public.addresses" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if c.RollbackSQL != parsed.Rollback() || !extstats.Undoes(parsed, c.RollbackSQL) {
		t.Fatalf("rollback %q does not undo %q", c.RollbackSQL, c.ForwardSQL)
	}
	if !slices.Contains(c.EvidenceIDs, "plan/query/202") ||
		!slices.Contains(c.EvidenceIDs, queryID("202")) {
		t.Fatalf("evidence = %v, want the plan and the query", c.EvidenceIDs)
	}
	if q := c.Prediction.Quantities["estimate_error"]; q != 500 {
		t.Fatalf("estimate_error = %v, want 500 (5000 actual / 10 estimated)", q)
	}
	if c.Kind != KindCorrelated || strings.Contains(c.Reason, "estimate drift") {
		t.Fatalf("kind %s reason %q", c.Kind, c.Reason)
	}
}

func TestGenerate_CorrelatedBoundary(t *testing.T) {
	for _, tc := range []struct {
		est, act float64
		flagged  bool
	}{{10, 100, false}, {10, 101, true}, {1000, 99, true}, {1000, 100, false}} {
		src := source(map[string]TableFacts{"public.addresses": addresses()})
		p := packOf(t, []query{addrQuery()}, addrStats(),
			[]workpack.PlanFact{scanPlan(tc.est, tc.act)})
		cands, _ := generate(t, src, p)
		if got := len(byAction(cands, "create_statistics")) == 1; got != tc.flagged {
			t.Errorf("est %v act %v flagged=%v, want %v", tc.est, tc.act, got, tc.flagged)
		}
	}
}

// Without actual rows (no auto_explain: Neon, Lakebase) the rule uses
// the estimate against the rows each call returned, and says so.
func TestGenerate_CorrelatedEstimateDriftOnly(t *testing.T) {
	src := source(map[string]TableFacts{"public.addresses": addresses()})
	p := packOf(t, []query{addrQuery()}, addrStats(), []workpack.PlanFact{scanPlan(10, -1)})
	cands, _ := generate(t, src, p)
	stx := byAction(cands, "create_statistics")
	if len(stx) != 1 || !strings.Contains(stx[0].Reason, "estimate drift only") {
		t.Fatalf("candidates = %+v, want one saying estimate drift only", cands)
	}
	if q := stx[0].Prediction.Quantities["estimate_error"]; q != 500 {
		t.Fatalf("drift = %v, want 500 (5000 rows a call / 10 estimated)", q)
	}
}

func TestGenerate_CorrelatedNeedsTwoEqualityColumnsAndAPlan(t *testing.T) {
	src := source(map[string]TableFacts{"public.addresses": addresses()})
	one := addrQuery()
	one.text = "SELECT id FROM public.addresses WHERE city = $1 AND zip > $2"
	p := packOf(t, []query{one}, addrStats(), []workpack.PlanFact{scanPlan(10, 5000)})
	if cands, _ := generate(t, src, p); len(cands) != 0 {
		t.Fatalf("one equality column gave %+v", cands)
	}
	noPlan := packOf(t, []query{addrQuery()}, addrStats(), nil)
	cands, cov := generate(t, src, noPlan)
	if len(cands) != 0 {
		t.Fatalf("no plan gave %+v", cands)
	}
	found := false
	for _, n := range cov.Notes {
		found = found || strings.Contains(n, "202") && strings.Contains(n, "no captured plan")
	}
	if !found {
		t.Fatalf("notes %q do not say query 202 has no captured plan", cov.Notes)
	}
}

// The ANALYZE the generator proposes is one the executor runs.
func TestAnalyzeStepIsExecutable(t *testing.T) {
	sql := analyzeSQL("public", "users", "deleted_at")
	if got := executor.ActionTypeForSQL(sql); got != "analyze_table" {
		t.Fatalf("%q classifies as %q", sql, got)
	}
	if err := executor.ValidateExecutorSQL(sql); err != nil {
		t.Fatalf("ValidateExecutorSQL(%q) = %v", sql, err)
	}
}

// A column with no more distinct values than its target has no histogram:
// all its values are in the most-common list, so a perfect correlation (a
// single repeated value) is no time-ordered cliff.
func TestGenerate_TimeOrderedNeedsAHistogram(t *testing.T) {
	users := clerkUsers()
	c := col(0.5, 1)
	c.Correlation = f64(1)
	users.Columns["status"] = c
	users.ModSinceAnalyze = 900_000
	q := clerkQuery()
	q.text = "SELECT id FROM public.users WHERE status = $1"
	src := source(map[string]TableFacts{"public.users": users})
	if cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil)); len(cands) != 0 {
		t.Fatalf("a one-value column gave %+v", cands)
	}
	c.NDistinct = 101
	users.Columns["status"] = c
	cands, _ := generate(t, src, packOf(t, []query{q}, clerkStats(), nil))
	if steps := byAction(cands, executor.ActionTypeSetColumnStatistics); len(steps) != 1 ||
		steps[0].Kind != KindTimeOrdered {
		t.Fatalf("101 distinct values past target 100: %+v, want one time-ordered step", cands)
	}
}
