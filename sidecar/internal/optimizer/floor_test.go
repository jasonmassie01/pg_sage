package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-12: with the tuning agent inactive (no LLM, breaker open, daily
// budget spent) no producer emitted missing_index advice. The floor
// enumerates deterministic candidates from the statements of seq-scan-heavy
// tables and admits them only through Optimizer.Admit.

type fakeFloorOpt struct {
	mu        sync.Mutex
	cold      bool
	contexts  map[string]TableContext
	ctxErr    map[string]error
	outcome   func(rec Recommendation) Admission
	admitted  []Recommendation
	ctxCalls  int
	coldCalls int
}

func (f *fakeFloorOpt) ColdStart(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coldCalls++
	return f.cold
}

func (f *fakeFloorOpt) TableContext(_ context.Context, _ *collector.Snapshot,
	table string) (TableContext, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxCalls++
	if err := f.ctxErr[table]; err != nil {
		return TableContext{}, false, err
	}
	tc, ok := f.contexts[table]
	return tc, ok, nil
}

func (f *fakeFloorOpt) Admit(_ context.Context, rec Recommendation, tc TableContext,
) Admission {
	f.mu.Lock()
	f.admitted = append(f.admitted, rec)
	out := f.outcome
	f.mu.Unlock()
	if rec.Table != tc.Schema+"."+tc.Table {
		return Admission{Rec: rec, Outcome: AdmitInvalid, Reason: "wrong table"}
	}
	if out != nil {
		return out(rec)
	}
	rec.WhatIf = WhatIfVerified
	return Admission{Rec: rec, Outcome: AdmitAccepted}
}

func (f *fakeFloorOpt) admitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.admitted)
}

type floorLogs struct {
	mu    sync.Mutex
	lines []string
}

func (l *floorLogs) fn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *floorLogs) find(parts ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

// floorTC is a 100k-row table with known statistics: customer_id and
// region are selective, status is not (60% one value), created_at is a
// range column, note has no statistics.
func floorTC(table string, stmts ...string) TableContext {
	tc := TableContext{Schema: "public", Table: table, LiveTuples: 100000,
		ColumnsKnown: true,
		Columns: []ColumnInfo{{Name: "id"}, {Name: "customer_id"}, {Name: "region"},
			{Name: "status"}, {Name: "created_at"}, {Name: "note"}, {Name: "edge"},
			{Name: "over"}, {Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}, {Name: "e"}},
		ColStats: []ColStat{
			{Column: "customer_id", NDistinct: -0.5},
			{Column: "region", MostCommonFreqs: []float64{0.05, 0.04}},
			{Column: "status", MostCommonFreqs: []float64{0.6, 0.3}},
			{Column: "created_at", NDistinct: -1},
			{Column: "edge", MostCommonFreqs: []float64{floorMaxSelectivity}},
			{Column: "over", MostCommonFreqs: []float64{floorMaxSelectivity + 0.0001}},
			{Column: "a", NDistinct: 1000}, {Column: "b", NDistinct: 1000},
			{Column: "c", NDistinct: 1000}, {Column: "d", NDistinct: 1000},
			{Column: "e", NDistinct: 1000},
		},
		Indexes: []IndexInfo{{Name: table + "_pkey", IsUnique: true, IsValid: true,
			Definition: "CREATE UNIQUE INDEX " + table + "_pkey ON public." + table +
				" USING btree (id)"}},
	}
	for i, s := range stmts {
		tc.Queries = append(tc.Queries, QueryInfo{QueryID: int64(i + 1), Text: s,
			Calls: int64(1000 - 100*i), MeanTimeMs: 5})
	}
	return tc
}

func fingerprints(recs []Recommendation) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, strings.TrimPrefix(r.FindingIdentifier(), r.Table+"|"))
	}
	return out
}

func TestFloorCandidates_SingleColumnOnTheMostFrequentSelectiveEquality(t *testing.T) {
	tc := floorTC("orders",
		"SELECT id, total FROM public.orders WHERE customer_id = $1",
		"SELECT id FROM public.orders WHERE region = $1",
		"SELECT id FROM public.orders WHERE status = $1")
	recs := floorCandidates(tc)
	got := fingerprints(recs)
	if strings.Join(got, ";") != "btree(customer_id);btree(region)" {
		t.Fatalf("candidates = %v, want customer_id then region (status is not selective)",
			got)
	}
	r := recs[0]
	if r.Table != "public.orders" || r.Category != OptimizerCategory || r.IndexType != "btree" {
		t.Fatalf("candidate = %+v", r)
	}
	if !strings.HasPrefix(r.DDL, "CREATE INDEX CONCURRENTLY ") ||
		!strings.Contains(r.DDL, " ON public.orders (customer_id)") {
		t.Fatalf("DDL = %q", r.DDL)
	}
	spec, err := ParseIndexDDL(r.DDL)
	if err != nil || spec.Name == "" || len(spec.Name) > 63 {
		t.Fatalf("the DDL needs a valid name for a deterministic rollback: %+v %v", spec, err)
	}
	if !strings.Contains(r.Rationale, "customer_id") || !strings.Contains(r.Rationale, "1000") {
		t.Fatalf("rationale names the column and its calls: %q", r.Rationale)
	}
}

func TestFloorCandidates_CompositeEqualityColumnsThenOneRange(t *testing.T) {
	tc := floorTC("orders",
		"SELECT * FROM public.orders WHERE customer_id = $1 AND created_at >= $2 "+
			"ORDER BY created_at LIMIT $3")
	got := fingerprints(floorCandidates(tc))
	// The composite serves every customer_id lookup: the single-column
	// index it subsumes is not proposed beside it.
	if strings.Join(got, ";") != "btree(customer_id,created_at)" {
		t.Fatalf("candidates = %v", got)
	}
	tc = floorTC("orders",
		"SELECT * FROM public.orders WHERE created_at BETWEEN $1 AND $2 AND region = $3")
	if got := fingerprints(floorCandidates(tc)); strings.Join(got, ";") !=
		"btree(region,created_at)" {
		t.Fatalf("BETWEEN is a range; equality leads: %v", got)
	}
}

func TestFloorCandidates_SelectivityCutoffBoundary(t *testing.T) {
	tc := floorTC("orders", "SELECT id FROM public.orders WHERE edge = $1",
		"SELECT id FROM public.orders WHERE over = $1",
		"SELECT id FROM public.orders WHERE status = $1",
		"SELECT id FROM public.orders WHERE note = $1")
	got := fingerprints(floorCandidates(tc))
	if strings.Join(got, ";") != "btree(edge)" {
		t.Fatalf("only a column at or under the %.2f cutoff qualifies (and one without "+
			"statistics never does): %v", floorMaxSelectivity, got)
	}
}

func TestFloorCandidates_AtMostThreePerTable(t *testing.T) {
	tc := floorTC("orders", "SELECT id FROM public.orders WHERE a = $1",
		"SELECT id FROM public.orders WHERE b = $1", "SELECT id FROM public.orders WHERE c = $1",
		"SELECT id FROM public.orders WHERE d = $1", "SELECT id FROM public.orders WHERE e = $1")
	got := fingerprints(floorCandidates(tc))
	if strings.Join(got, ";") != "btree(a);btree(b);btree(c)" {
		t.Fatalf("candidates = %v, want the three most-called", got)
	}
}

func TestFloorCandidates_UnparseableStatementsContributeNothing(t *testing.T) {
	for _, stmt := range []string{
		"SELEC id FRM public.orders WHER customer_id = $1",
		"SELECT id FROM public.orders WHERE (customer_id = $1",
		"SELECT id FROM public.orders WHERE customer_id = $1 OR region = $2",
		"SELECT id FROM public.orders WHERE id IN (SELECT id FROM public.orders " +
			"WHERE region = $1)",
		"SELECT id FROM public.orders",
		"",
		"INSERT INTO public.orders (customer_id) VALUES ($1)",
		"SELECT id FROM public.orders WHERE note = $1 -- customer_id = $2",
		"SELECT id FROM public.orders WHERE note = 'customer_id = $1'",
	} {
		if got := floorCandidates(floorTC("orders", stmt)); len(got) != 0 {
			t.Errorf("%q gave candidates %v", stmt, fingerprints(got))
		}
	}
	// An UPDATE's SET list is not a filter: only its WHERE clause counts.
	tc := floorTC("orders", "UPDATE public.orders SET region = $1 WHERE customer_id = $2")
	if got := fingerprints(floorCandidates(tc)); strings.Join(got, ";") !=
		"btree(customer_id)" {
		t.Fatalf("UPDATE candidates = %v", got)
	}
}

func TestFloorCandidates_ExistingCoveringIndexGivesNone(t *testing.T) {
	tc := floorTC("orders", "SELECT id FROM public.orders WHERE customer_id = $1")
	tc.Indexes = append(tc.Indexes, IndexInfo{Name: "orders_cust_created", IsValid: true,
		Definition: "CREATE INDEX orders_cust_created ON public.orders USING btree " +
			"(customer_id, created_at)"})
	if got := floorCandidates(tc); len(got) != 0 {
		t.Fatalf("an existing index already serves the lookup: %v", fingerprints(got))
	}
}

func TestFloorCandidates_EmptyContext(t *testing.T) {
	if got := floorCandidates(TableContext{}); got != nil {
		t.Fatalf("an empty context has no candidates: %v", got)
	}
	if got := floorCandidates(floorTC("orders")); got != nil {
		t.Fatalf("a table without statements has no candidates: %v", got)
	}
}

func floorWith(opt *fakeFloorOpt, logs *floorLogs) *Floor {
	return &Floor{opt: opt, logFn: logs.fn}
}

func oneTableOpt(tables ...string) *fakeFloorOpt {
	opt := &fakeFloorOpt{contexts: map[string]TableContext{}}
	for _, t := range tables {
		name := strings.TrimPrefix(t, "public.")
		opt.contexts[t] = floorTC(name, "SELECT id FROM public."+name+" WHERE customer_id = $1")
	}
	return opt
}

// Nil/empty: no seq_scan_heavy table means no work and no query.
func TestFloorRun_NoTablesNoWork(t *testing.T) {
	opt := oneTableOpt("public.orders")
	logs := &floorLogs{}
	for _, tables := range [][]string{nil, {}} {
		res := floorWith(opt, logs).Run(context.Background(), &collector.Snapshot{}, tables,
			nil)
		if res.Ran || len(res.Admitted) != 0 || len(res.Incomplete) != 0 {
			t.Fatalf("result = %+v", res)
		}
	}
	if opt.coldCalls != 0 || opt.ctxCalls != 0 || opt.admitCount() != 0 {
		t.Fatalf("no tables still queried: cold %d ctx %d admit %d", opt.coldCalls,
			opt.ctxCalls, opt.admitCount())
	}
	var nilFloor *Floor
	if res := nilFloor.Run(context.Background(), nil, []string{"public.orders"}, nil); res.Ran {
		t.Fatal("a nil floor runs nothing")
	}
}

func TestFloorRun_AdmitsOneFindingPerTable(t *testing.T) {
	opt := oneTableOpt("public.orders")
	res := floorWith(opt, &floorLogs{}).Run(context.Background(), &collector.Snapshot{},
		[]string{"public.orders"}, nil)
	if !res.Ran || len(res.Admitted) != 1 || len(res.Incomplete) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if id := res.Admitted[0].Rec.FindingIdentifier(); id != "public.orders|btree(customer_id)" {
		t.Fatalf("identifier = %q", id)
	}
}

// Boundary: 11 qualifying tables give at most 10 candidates in the cycle;
// the table the cap cut is incomplete (its open findings must not resolve).
func TestFloorRun_CycleCap(t *testing.T) {
	var tables []string
	for i := range 11 {
		tables = append(tables, fmt.Sprintf("public.t%02d", i))
	}
	opt := oneTableOpt(tables...)
	res := floorWith(opt, &floorLogs{}).Run(context.Background(), &collector.Snapshot{},
		tables, nil)
	if opt.admitCount() != floorMaxPerCycle || len(res.Admitted) != floorMaxPerCycle {
		t.Fatalf("admitted %d (%d calls), want %d", len(res.Admitted), opt.admitCount(),
			floorMaxPerCycle)
	}
	if strings.Join(res.Incomplete, ",") != "public.t10" {
		t.Fatalf("incomplete = %v, want the 11th table", res.Incomplete)
	}
	for _, a := range res.Admitted {
		if a.Rec.Table == "public.t10" {
			t.Fatal("the 11th table was admitted past the cap")
		}
	}
}

// Error propagation: an Admit failure is logged with the table and the
// candidate, and the cycle goes on with the next table; a table whose
// context cannot be read is logged and incomplete.
func TestFloorRun_AdmitAndContextErrorsAreLoggedAndTheCycleContinues(t *testing.T) {
	opt := oneTableOpt("public.bad", "public.good")
	opt.ctxErr = map[string]error{"public.broken": errors.New("statement timeout")}
	opt.outcome = func(rec Recommendation) Admission {
		if rec.Table == "public.bad" {
			return Admission{Rec: rec, Outcome: AdmitInvalid, Reason: "column vanished"}
		}
		rec.WhatIf = WhatIfVerified
		return Admission{Rec: rec, Outcome: AdmitAccepted}
	}
	logs := &floorLogs{}
	res := floorWith(opt, logs).Run(context.Background(), &collector.Snapshot{},
		[]string{"public.bad", "public.broken", "public.good"}, nil)
	if len(res.Admitted) != 1 || res.Admitted[0].Rec.Table != "public.good" {
		t.Fatalf("admitted = %+v", res.Admitted)
	}
	if !logs.find("public.bad", "CREATE INDEX CONCURRENTLY", "column vanished") {
		t.Fatalf("the refusal names table, candidate and reason: %v", logs.lines)
	}
	if !logs.find("WARN", "public.broken", "statement timeout") {
		t.Fatalf("the context error is logged with the table: %v", logs.lines)
	}
	if strings.Join(res.Incomplete, ",") != "public.broken" {
		t.Fatalf("incomplete = %v", res.Incomplete)
	}
}

func TestFloorRun_RejectedAndMeasuredAreNotAdmitted(t *testing.T) {
	for _, outcome := range []AdmissionOutcome{AdmitRejected, AdmitMeasured} {
		opt := oneTableOpt("public.orders")
		opt.outcome = func(rec Recommendation) Admission {
			return Admission{Rec: rec, Outcome: outcome, Reason: "below 10%"}
		}
		logs := &floorLogs{}
		res := floorWith(opt, logs).Run(context.Background(), &collector.Snapshot{},
			[]string{"public.orders"}, nil)
		if len(res.Admitted) != 0 || !res.Ran || len(res.Incomplete) != 0 {
			t.Fatalf("%s: result = %+v", outcome, res)
		}
		if !logs.find("public.orders", "below 10%") {
			t.Fatalf("%s: the outcome is logged: %v", outcome, logs.lines)
		}
	}
}

// A shape with an open finding (the agent's) is never proposed again.
func TestFloorRun_SkipsOpenShapes(t *testing.T) {
	opt := oneTableOpt("public.orders")
	res := floorWith(opt, &floorLogs{}).Run(context.Background(), &collector.Snapshot{},
		[]string{"public.orders"}, map[string]bool{"public.orders|btree(customer_id)": true})
	if opt.admitCount() != 0 || len(res.Admitted) != 0 || !res.Ran {
		t.Fatalf("an open shape was re-admitted: %d calls, %+v", opt.admitCount(), res)
	}
}

func TestFloorRun_ColdStartAndUnknownTables(t *testing.T) {
	opt := oneTableOpt("public.orders")
	opt.cold = true
	res := floorWith(opt, &floorLogs{}).Run(context.Background(), &collector.Snapshot{},
		[]string{"public.orders"}, nil)
	if res.Ran || opt.ctxCalls != 0 {
		t.Fatalf("a cold start judges nothing: %+v (ctx %d)", res, opt.ctxCalls)
	}
	opt.cold = false
	res = floorWith(opt, &floorLogs{}).Run(context.Background(), &collector.Snapshot{},
		[]string{"public.gone"}, nil)
	if !res.Ran || len(res.Admitted) != 0 || len(res.Incomplete) != 0 {
		t.Fatalf("a table the snapshot lacks is skipped: %+v", res)
	}
}

// Concurrency: two runtimes (fleet) run their floors in parallel with no
// shared state.
func TestFloorRun_ParallelRuntimesShareNothing(t *testing.T) {
	var wg sync.WaitGroup
	results := make([][]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			table := fmt.Sprintf("public.db%d", i)
			opt := oneTableOpt(table)
			for range 5 {
				res := floorWith(opt, &floorLogs{}).Run(context.Background(),
					&collector.Snapshot{}, []string{table}, nil)
				for _, a := range res.Admitted {
					results[i] = append(results[i], a.Rec.FindingIdentifier())
				}
			}
		}(i)
	}
	wg.Wait()
	for i, ids := range results {
		want := fmt.Sprintf("public.db%d|btree(customer_id)", i)
		if len(ids) != 5 {
			t.Fatalf("runtime %d admitted %v", i, ids)
		}
		for _, id := range ids {
			if id != want {
				t.Fatalf("runtime %d saw %q, want only %q", i, id, want)
			}
		}
	}
}
