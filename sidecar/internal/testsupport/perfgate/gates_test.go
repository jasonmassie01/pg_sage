package perfgate

import (
	"strings"
	"testing"
	"time"
)

func steadyPhase() Phase {
	return Phase{Name: "steady", Steady: true, Window: 90 * time.Second, Cycles: 6}
}

func TestEvaluateCleanRunHasNoOffenders(t *testing.T) {
	p := steadyPhase()
	p.Tables = []TableDelta{{Name: "sage.findings", LiveRows: 200000, IdxScans: 40,
		RowsWritten: 60}}
	p.Statements = []Statement{{QueryID: 1, Query: "SELECT 1 FROM sage.findings",
		Calls: 6, TotalMs: 30, MeanMs: 5, MaxMs: 9}}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("clean run produced offenders: %+v", got)
	}
}

func TestEvaluateSeqScanAboveThresholdIsConfirmedOffender(t *testing.T) {
	p := steadyPhase()
	p.Tables = []TableDelta{
		{Name: "sage.decision", LiveRows: 200000, SeqScans: 12, SeqTupRead: 2400000},
		{Name: "sage.config", LiveRows: 300, SeqScans: 50, SeqTupRead: 15000},
		{Name: "sage.findings", LiveRows: 200000, SeqScans: 1, SeqTupRead: 200000},
	}
	p.Plans = []PlanResult{
		{Statement: Statement{QueryID: 7, Query: "SELECT count(*) FROM sage.decision",
			TotalMs: 900}, SeqScans: []PlanScan{{Schema: "sage", Relation: "decision"}}},
		{Statement: Statement{QueryID: 8, Query: "SELECT * FROM sage.decision d",
			TotalMs: 50}, SeqScans: []PlanScan{{Schema: "sage", Relation: "decision"}}},
	}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("offenders = %+v, want decision and findings only", got)
	}
	// Ranked by rows read sequentially: decision (2.4M) before findings.
	if got[0].Subject != "sage.decision" || got[1].Subject != "sage.findings" {
		t.Fatalf("ranking = %s, %s", got[0].Subject, got[1].Subject)
	}
	if got[0].Gate != GateSeqScan || got[0].Measured != 2400000 {
		t.Fatalf("decision offender = %+v", got[0])
	}
	// Attribution names the costlier suspect statement first.
	if !strings.Contains(got[0].Detail, "queryid 7") ||
		strings.Index(got[0].Detail, "queryid 7") > strings.Index(got[0].Detail, "queryid 8") {
		t.Fatalf("decision detail does not rank suspects: %q", got[0].Detail)
	}
	if !strings.Contains(got[1].Detail, "no captured statement") {
		t.Fatalf("unattributed seq scan detail = %q", got[1].Detail)
	}
}

func TestEvaluateSeqScanBoundaryIsExclusive(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Tables = []TableDelta{
		{Name: "sage.at", LiveRows: b.SeqScanMinRows, SeqScans: 3, SeqTupRead: 3},
		{Name: "sage.above", LiveRows: b.SeqScanMinRows + 1, SeqScans: 1, SeqTupRead: 1},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 1 || got[0].Subject != "sage.above" {
		t.Fatalf("boundary offenders = %+v, want only sage.above", got)
	}
}

func TestEvaluateStatementAndCycleTimeBudgets(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Statements = []Statement{
		{QueryID: 1, Query: "SELECT slow", Calls: 2, TotalMs: 2 * (b.StatementMeanMs + 1),
			MeanMs: b.StatementMeanMs + 1, MaxMs: b.StatementMeanMs + 2},
		{QueryID: 2, Query: "SELECT at_budget", Calls: 1, TotalMs: b.StatementMeanMs,
			MeanMs: b.StatementMeanMs, MaxMs: b.StatementMeanMs},
		{QueryID: 3, Query: "SELECT heavy", Calls: 600,
			TotalMs: b.CycleDBTimeMs * float64(p.Cycles), MeanMs: 1, MaxMs: 3},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var mean, cycle int
	for _, o := range got {
		switch o.Gate {
		case GateStatementMean:
			mean++
			if !strings.Contains(o.Subject, "queryid 1") {
				t.Fatalf("mean offender = %+v", o)
			}
		case GateCycleDBTime:
			cycle++
			want := (b.CycleDBTimeMs*float64(p.Cycles) + 2*(b.StatementMeanMs+1) +
				b.StatementMeanMs) / float64(p.Cycles)
			if o.Measured != want {
				t.Fatalf("cycle DB time = %v, want %v", o.Measured, want)
			}
		}
	}
	if mean != 1 || cycle != 1 {
		t.Fatalf("mean offenders %d, cycle offenders %d; all: %+v", mean, cycle, got)
	}
}

func TestEvaluateRowsWrittenPerCycle(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Tables = []TableDelta{
		{Name: "sage.runway_samples", RowsWritten: 5000 * int64(p.Cycles)},
		{Name: "sage.snapshots", RowsWritten: b.RowsWrittenPerCycle * int64(p.Cycles)},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 1 || got[0].Gate != GateRowsWritten ||
		got[0].Subject != "sage.runway_samples" || got[0].Measured != 5000 {
		t.Fatalf("rows written offenders = %+v", got)
	}
	// Mutation audit: one row per cycle over the budget is already over.
	p.Tables = []TableDelta{{Name: "sage.just_over",
		RowsWritten: (b.RowsWrittenPerCycle + 1) * int64(p.Cycles)}}
	got, err = Evaluate([]Phase{p}, b)
	if err != nil || len(got) != 1 || got[0].Subject != "sage.just_over" {
		t.Fatalf("one row over budget: %+v (%v)", got, err)
	}
}

func TestEvaluateWarmupSkipsSteadyStateBudgets(t *testing.T) {
	b := DefaultBudgets()
	warm := Phase{Name: "warmup", Window: 45 * time.Second, Cycles: 0}
	warm.Tables = []TableDelta{{Name: "sage.schema_baseline", RowsWritten: 25000}}
	warm.Statements = []Statement{{QueryID: 9, Query: "SELECT big FROM sage.x",
		Calls: 1, TotalMs: 4000, MeanMs: 4000, MaxMs: 4000}}
	got, err := Evaluate([]Phase{warm}, b)
	if err != nil {
		t.Fatalf("evaluate warmup: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("warmup charged steady-state budgets: %+v", got)
	}
}

func TestEvaluateCatalogStatementsAndTimeouts(t *testing.T) {
	b := DefaultBudgets()
	warm := Phase{Name: "warmup", Window: time.Minute}
	warm.Statements = []Statement{
		{QueryID: 4, Query: "SELECT c.relname FROM pg_class c JOIN pg_index i ON true",
			Calls: 1, TotalMs: 800, MeanMs: 800, MaxMs: 800},
		{QueryID: 5, Query: "SELECT * FROM sage.findings", Calls: 1, TotalMs: 800,
			MeanMs: 800, MaxMs: 800},
	}
	warm.Timeouts = []string{"collector: indexes unavailable: canceling statement " +
		"due to statement timeout", "collector: indexes unavailable: canceling statement " +
		"due to statement timeout"}
	got, err := Evaluate([]Phase{warm}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("offenders = %+v, want one catalog statement and one timeout", got)
	}
	if got[0].Gate != GateCatalogMax || !strings.Contains(got[0].Subject, "queryid 4") {
		t.Fatalf("catalog offender = %+v", got[0])
	}
	if got[1].Gate != GateTimeout || got[1].Measured != 2 {
		t.Fatalf("timeout offender = %+v (deduplicated count expected 2)", got[1])
	}
}

func TestEvaluateAPIEndpoints(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Endpoints = []Endpoint{
		{Path: "/api/v1/findings", Status: 200, Duration: 20 * time.Millisecond},
		{Path: "/api/v1/cases", Status: 500, Duration: 5 * time.Millisecond},
		{Path: "/api/v1/value", Status: 200,
			Duration: time.Duration(b.EndpointMaxMs+1) * time.Millisecond},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("endpoint offenders = %+v", got)
	}
	subjects := got[0].Subject + "," + got[1].Subject
	if !strings.Contains(subjects, "/api/v1/cases") || !strings.Contains(subjects, "/api/v1/value") {
		t.Fatalf("endpoint offenders = %s", subjects)
	}
}

func TestEvaluateRejectsBrokenInput(t *testing.T) {
	if _, err := Evaluate(nil, DefaultBudgets()); err == nil ||
		!strings.Contains(err.Error(), "no phases") {
		t.Fatalf("nil phases: err = %v", err)
	}
	zero := steadyPhase()
	zero.Cycles = 0
	if _, err := Evaluate([]Phase{zero}, DefaultBudgets()); err == nil ||
		!strings.Contains(err.Error(), "cycles") {
		t.Fatalf("steady phase without cycles: err = %v", err)
	}
	if _, err := Evaluate([]Phase{steadyPhase()}, Budgets{}); err == nil ||
		!strings.Contains(err.Error(), "budget") {
		t.Fatalf("zero budgets: err = %v", err)
	}
}

func TestEvaluateRanksGatesInDocumentedOrder(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Tables = []TableDelta{{Name: "sage.a", LiveRows: 1e6, SeqScans: 1, SeqTupRead: 1e6,
		RowsWritten: 1e6}}
	p.Statements = []Statement{{QueryID: 1, Query: "SELECT x FROM pg_class",
		Calls: 1, TotalMs: 1e5, MeanMs: 1e5, MaxMs: 1e5}}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var order []Gate
	for _, o := range got {
		order = append(order, o.Gate)
	}
	want := []Gate{GateSeqScan, GateStatementMean, GateCycleDBTime, GateRowsWritten,
		GateCatalogMax}
	if len(order) != len(want) {
		t.Fatalf("gates = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("gates = %v, want %v", order, want)
		}
	}
}

func TestIsCatalogQuery(t *testing.T) {
	yes := []string{
		"SELECT relname FROM pg_class",
		"/* pg_sage */ SELECT * FROM pg_stat_user_tables",
		"SELECT 1 FROM pg_catalog.pg_namespace n",
		"select * from information_schema.columns",
		"SELECT s.* FROM pg_sequences s",
		"SELECT * FROM pg_stat_statements",
	}
	no := []string{
		"SELECT * FROM sage.findings",
		"SELECT pg_total_relation_size($1)",
		"INSERT INTO sage.snapshots (data) VALUES ($1)",
		"",
	}
	for _, q := range yes {
		if !IsCatalogQuery(q) {
			t.Errorf("%q not classified as catalog", q)
		}
	}
	for _, q := range no {
		if IsCatalogQuery(q) {
			t.Errorf("%q classified as catalog", q)
		}
	}
}

// Post-test audit: the live delta test only asserts "at least one" scan,
// which an absolute (non-delta) reading would also satisfy.
func TestTableStatsDeltaIsExact(t *testing.T) {
	before := TableStats{
		"sage.a": {LiveRows: 10, SeqScan: 5, SeqTupRead: 50, IdxScan: 7, Written: 100},
	}
	after := TableStats{
		"sage.a": {LiveRows: 12, SeqScan: 6, SeqTupRead: 62, IdxScan: 9, Written: 103},
		"sage.b": {LiveRows: 3, SeqScan: 1, SeqTupRead: 3, IdxScan: 0, Written: 3},
	}
	got := after.Delta(before)
	want := []TableDelta{
		{Name: "sage.a", LiveRows: 12, SeqScans: 1, SeqTupRead: 12, IdxScans: 2, RowsWritten: 3,
			Relations: 1},
		{Name: "sage.b", LiveRows: 3, SeqScans: 1, SeqTupRead: 3, RowsWritten: 3, Relations: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("delta = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(TableStats{}.Delta(before)) != 0 {
		t.Fatal("empty reading produced deltas")
	}
}
