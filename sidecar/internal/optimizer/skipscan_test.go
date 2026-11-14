package optimizer

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// market-15b: PostgreSQL 18 btree skip scan lets (tenant_id, created_at)
// serve WHERE created_at = $1 when tenant_id has few distinct values. A
// single-column (created_at) candidate is then dropped, but only on plan
// evidence: every top statement's generic plan must already scan the
// composite with an Index Cond on created_at.

// No concurrent-access tests here: the optimizer judges one table per
// goroutine, and rejection memory has its own concurrency tests.

const (
	skipIndexName = "t_tenant_created"
	skipIndexDef  = "CREATE INDEX t_tenant_created ON public.t USING btree (tenant_id, created_at)"
	createdAtDDL  = "CREATE INDEX CONCURRENTLY t_created_idx ON public.t (created_at)"
	createdAtStmt = "SELECT id FROM t WHERE created_at = $1"
)

// skipScanPlanJSON is a PG 18 generic plan of createdAtStmt: an index
// scan of the composite whose only Index Cond is on created_at.
const skipScanPlanJSON = `[{"Plan": {"Node Type": "Index Scan", "Parallel Aware": false,
  "Scan Direction": "Forward", "Index Name": "t_tenant_created", "Relation Name": "t",
  "Alias": "t", "Startup Cost": 0.29, "Total Cost": 52.4, "Plan Rows": 9,
  "Index Cond": "(created_at = $1)"}}]`

const seqScanPlanJSON = `[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "t",
  "Total Cost": 1693.0, "Filter": "(created_at = $1)"}}]`

// fakeGenericPlans answers genericPlans with a fixed plan per statement
// (or err) and records each call's statements.
type fakeGenericPlans struct {
	mu    sync.Mutex
	plans map[string]string
	err   error
	calls [][]string
}

func (f *fakeGenericPlans) genericPlans(_ context.Context, stmts []string) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), stmts...))
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]byte, len(stmts))
	for i, s := range stmts {
		p, ok := f.plans[s]
		if !ok {
			p = seqScanPlanJSON
		}
		out[i] = []byte(p)
	}
	return out, nil
}

func (f *fakeGenericPlans) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// skipTable is a PG 18 table with (tenant_id, created_at) and 12 tenants.
func skipTable() TableContext {
	return TableContext{Schema: "public", Table: "t", PGMajor: 18, LiveTuples: 100000,
		ColumnsKnown: true, IndexCount: 1, WriteRate: 5, Workload: "oltp_read",
		Columns: []ColumnInfo{{Name: "id", Type: "bigint"}, {Name: "tenant_id", Type: "integer"},
			{Name: "created_at", Type: "timestamp with time zone"}, {Name: "note", Type: "text"}},
		Indexes: []IndexInfo{{Name: skipIndexName, Definition: skipIndexDef, IsValid: true}},
		ColStats: []ColStat{{Column: "tenant_id", NDistinct: 12},
			{Column: "created_at", NDistinct: -1}},
		Queries: []QueryInfo{{QueryID: 7, Text: createdAtStmt, Calls: 500, MeanTimeMs: 3,
			TotalTimeMs: 1500}},
	}
}

func skipOptimizer(store *memStore, plans *fakeGenericPlans, res WhatIfResult,
) (*Optimizer, *countingWhatIf, *logRecorder) {
	o, w, logs := admissionOptimizer(store, res)
	if plans != nil {
		o.skipScanPlans = plans
	}
	return o, w, logs
}

func servedPlans() *fakeGenericPlans {
	return &fakeGenericPlans{plans: map[string]string{createdAtStmt: skipScanPlanJSON}}
}

var verifiedResult = WhatIfResult{Measured: 1, Improvement: 60, SizeBytes: 8192}

// Happy path: every plan scans the composite with an Index Cond on
// created_at, so the candidate is dropped, named and remembered.
func TestSkipScan_ServedCandidateIsDropped(t *testing.T) {
	store, plans := newMemStore(), servedPlans()
	o, w, _ := skipOptimizer(store, plans, verifiedResult)
	a := o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	if a.Outcome != AdmitRejected || a.Reason != "served_by_skip_scan:t_tenant_created" {
		t.Fatalf("admission = %s %q, want rejected served_by_skip_scan:t_tenant_created",
			a.Outcome, a.Reason)
	}
	if w.calls.Load() != 0 {
		t.Fatalf("the what-if ran %d times on a candidate the plans show served",
			w.calls.Load())
	}
	if len(plans.calls) != 1 || !reflect.DeepEqual(plans.calls[0], []string{createdAtStmt}) {
		t.Fatalf("EXPLAIN calls = %q, want one with the created_at statement", plans.calls)
	}
	rows := store.snapshot()
	if store.records != 1 || len(rows) != 1 ||
		rows[0].Reason != "served_by_skip_scan:t_tenant_created" || rows[0].Table != "t" {
		t.Fatalf("remembered %d rows %+v, want one served_by_skip_scan row", store.records, rows)
	}
	if a.Rec.WhatIf != WhatIfRejected || a.Rec.Validated {
		t.Fatalf("rec verdict %q validated %t", a.Rec.WhatIf, a.Rec.Validated)
	}
}

func TestSkipScanCandidate_ReturnsTheServingIndex(t *testing.T) {
	ix, ok := skipScanCandidate(skipTable(), candidate(createdAtDDL))
	if !ok || ix.Name != skipIndexName || ix.Definition != skipIndexDef {
		t.Fatalf("skipScanCandidate = %+v %t, want %s", ix, ok, skipIndexName)
	}
}

// Boundaries: leading n_distinct 200 qualifies, 201 does not; PG 17 never
// qualifies; a negative n_distinct is a fraction of the live tuples.
func TestSkipScanCandidate_Boundaries(t *testing.T) {
	cases := []struct {
		name     string
		major    int
		distinct float64
		live     int64
		want     bool
	}{
		{"200 distinct", 18, 200, 100000, true},
		{"201 distinct", 18, 201, 100000, false},
		{"1 distinct", 18, 1, 100000, true},
		{"PG 17", 17, 12, 100000, false},
		{"PG 19", 19, 12, 100000, true},
		{"unknown major", 0, 12, 100000, false},
		{"-0.5 of 1000 is 500", 18, -0.5, 1000, false},
		{"-0.2 of 1000 is 200", 18, -0.2, 1000, true},
		{"-0.01 of 1000 is 10", 18, -0.01, 1000, true},
		{"negative with no live tuples", 18, -0.5, 0, false},
		{"zero n_distinct is unknown", 18, 0, 100000, false},
	}
	for _, c := range cases {
		tc := skipTable()
		tc.PGMajor, tc.LiveTuples = c.major, c.live
		tc.ColStats[0].NDistinct = c.distinct
		ix, ok := skipScanCandidate(tc, candidate(createdAtDDL))
		if ok != c.want || (ok && ix.Name != skipIndexName) {
			t.Errorf("%s: skipScanCandidate = %+v %t, want %t", c.name, ix, ok, c.want)
		}
	}
}

// Invalid input: only a valid btree with a plain leading key and the
// candidate's column second, and a single plain-column btree candidate,
// qualify.
func TestSkipScanCandidate_ShapesThatNeverQualify(t *testing.T) {
	existing := map[string]string{
		"expression leading key": "CREATE INDEX t_tenant_created ON public.t " +
			"USING btree (lower(note), created_at)",
		"non-btree": "CREATE INDEX t_tenant_created ON public.t USING gist (tenant_id, created_at)",
		"column third": "CREATE INDEX t_tenant_created ON public.t " +
			"USING btree (tenant_id, id, created_at)",
		"column leading": "CREATE INDEX t_tenant_created ON public.t " +
			"USING btree (created_at, tenant_id)",
		"single key": "CREATE INDEX t_tenant_created ON public.t USING btree (tenant_id)",
		"partial that does not serve": "CREATE INDEX t_tenant_created ON public.t " +
			"USING btree (tenant_id, created_at) WHERE (note IS NOT NULL)",
		"unparseable": "not an index definition",
	}
	for name, def := range existing {
		tc := skipTable()
		tc.Indexes[0].Definition = def
		if ix, ok := skipScanCandidate(tc, candidate(createdAtDDL)); ok {
			t.Errorf("%s: qualified through %+v", name, ix)
		}
	}
	tc := skipTable()
	tc.Indexes[0].IsValid = false
	if _, ok := skipScanCandidate(tc, candidate(createdAtDDL)); ok {
		t.Error("an invalid index qualified")
	}
	candidates := map[string]string{
		"composite candidate": "CREATE INDEX CONCURRENTLY x ON public.t (created_at, id)",
		"expression candidate": "CREATE INDEX CONCURRENTLY x ON public.t " +
			"((created_at::date))",
		"unique candidate": "CREATE UNIQUE INDEX CONCURRENTLY x ON public.t (created_at)",
		"hash candidate":   "CREATE INDEX CONCURRENTLY x ON public.t USING hash (created_at)",
		"include not carried": "CREATE INDEX CONCURRENTLY x ON public.t (created_at) " +
			"INCLUDE (note)",
		"unparseable": "CREATE INDEX CONCURRENTLY",
	}
	for name, ddl := range candidates {
		if ix, ok := skipScanCandidate(skipTable(), candidate(ddl)); ok {
			t.Errorf("%s: qualified through %+v", name, ix)
		}
	}
}

// A partial composite whose predicate the candidate implies, and an
// INCLUDE column the composite carries, still qualify.
func TestSkipScanCandidate_ServingPredicateAndInclude(t *testing.T) {
	tc := skipTable()
	tc.Indexes[0].Definition = "CREATE INDEX t_tenant_created ON public.t " +
		"USING btree (tenant_id, created_at) INCLUDE (note)"
	ddl := "CREATE INDEX CONCURRENTLY x ON public.t (created_at) INCLUDE (note)"
	if _, ok := skipScanCandidate(tc, candidate(ddl)); !ok {
		t.Error("a composite carrying the INCLUDE column did not qualify")
	}
	tc = skipTable()
	tc.Indexes[0].Definition = "CREATE INDEX t_tenant_created ON public.t " +
		"USING btree (tenant_id, created_at) WHERE (note IS NOT NULL)"
	ddl = "CREATE INDEX CONCURRENTLY x ON public.t (created_at) WHERE note IS NOT NULL"
	if _, ok := skipScanCandidate(tc, candidate(ddl)); !ok {
		t.Error("a composite with the candidate's own predicate did not qualify")
	}
}

// Nil/empty: no statistics row for the leading key, no indexes, or an
// empty context never qualify.
func TestSkipScanCandidate_MissingInputs(t *testing.T) {
	tc := skipTable()
	tc.ColStats = []ColStat{{Column: "created_at", NDistinct: -1}}
	if _, ok := skipScanCandidate(tc, candidate(createdAtDDL)); ok {
		t.Error("qualified without statistics for the leading key")
	}
	tc = skipTable()
	tc.Indexes = nil
	if _, ok := skipScanCandidate(tc, candidate(createdAtDDL)); ok {
		t.Error("qualified with no index on the table")
	}
	if _, ok := skipScanCandidate(TableContext{}, Recommendation{}); ok {
		t.Error("an empty context and candidate qualified")
	}
}

// The leading key's statistics may come from LeadingKeyStats, read for
// index keys the workload does not name.
func TestSkipScanCandidate_LeadingKeyStats(t *testing.T) {
	tc := skipTable()
	tc.ColStats = nil
	tc.LeadingKeyStats = []ColStat{{Column: "tenant_id", NDistinct: 12}}
	if _, ok := skipScanCandidate(tc, candidate(createdAtDDL)); !ok {
		t.Error("the leading key's statistics in LeadingKeyStats were not used")
	}
}

// Nil/empty: no statement filters the column, so no EXPLAIN runs and the
// candidate proceeds to the what-if.
func TestSkipScan_NoWorkloadStatementMeansNoExplain(t *testing.T) {
	for name, queries := range map[string][]QueryInfo{
		"no statements": nil,
		"other column": {{QueryID: 8, Text: "SELECT id FROM t WHERE tenant_id = $1",
			Calls: 50, MeanTimeMs: 1, TotalTimeMs: 50}},
		"longer name only": {{QueryID: 9, Text: "SELECT id FROM t WHERE created_at_old = $1",
			Calls: 50, MeanTimeMs: 1, TotalTimeMs: 50}},
	} {
		plans := servedPlans()
		o, w, _ := skipOptimizer(newMemStore(), plans, verifiedResult)
		tc := skipTable()
		tc.Queries = queries
		a := o.Admit(context.Background(), candidate(createdAtDDL), tc)
		if a.Outcome != AdmitAccepted || w.calls.Load() != 1 || plans.callCount() != 0 {
			t.Errorf("%s: admission %s %q, what-ifs %d, EXPLAIN calls %d", name, a.Outcome,
				a.Reason, w.calls.Load(), plans.callCount())
		}
	}
}

// Error propagation: a failed EXPLAIN keeps the candidate and logs one
// WARN naming the table.
func TestSkipScan_ExplainErrorKeepsTheCandidate(t *testing.T) {
	errInvalidSQL := &pgconn.PgError{Code: "42601", Message: "syntax error at or near \"$1\""}
	plans := &fakeGenericPlans{err: errInvalidSQL}
	o, w, logs := skipOptimizer(newMemStore(), plans, verifiedResult)
	a := o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	if a.Outcome != AdmitAccepted || a.Rec.WhatIf != WhatIfVerified || w.calls.Load() != 1 {
		t.Fatalf("admission %s %q (what-ifs %d), want accepted and measured", a.Outcome,
			a.Reason, w.calls.Load())
	}
	warns := logs.matching("WARN", "public.t", "syntax error")
	if len(warns) != 1 {
		t.Fatalf("WARN lines = %q, want one naming public.t and the error", logs.lines)
	}
}

// A plan that does not use the composite on created_at keeps the
// candidate: missing evidence never drops one.
func TestSkipScan_IncompleteEvidenceKeepsTheCandidate(t *testing.T) {
	second := "SELECT note FROM t WHERE created_at = $1 AND note = $2"
	cases := map[string]string{
		"seq scan": seqScanPlanJSON,
		"other index": strings.Replace(skipScanPlanJSON, `"t_tenant_created"`,
			`"t_other"`, 1),
		"cond on the leading key only": strings.Replace(skipScanPlanJSON,
			"(created_at = $1)", "(tenant_id = $1)", 1),
	}
	for name, plan := range cases {
		plans := &fakeGenericPlans{plans: map[string]string{createdAtStmt: skipScanPlanJSON,
			second: plan}}
		o, w, _ := skipOptimizer(newMemStore(), plans, verifiedResult)
		tc := skipTable()
		tc.Queries = append(tc.Queries, QueryInfo{QueryID: 8, Text: second, Calls: 90,
			MeanTimeMs: 2, TotalTimeMs: 180})
		a := o.Admit(context.Background(), candidate(createdAtDDL), tc)
		if a.Outcome != AdmitAccepted || w.calls.Load() != 1 || plans.callCount() != 1 {
			t.Errorf("%s: admission %s %q, what-ifs %d, EXPLAIN calls %d", name, a.Outcome,
				a.Reason, w.calls.Load(), plans.callCount())
		}
	}
}

// At most 3 statements are planned: the most-called ones naming the column.
func TestSkipScan_PlansTheTopThreeStatements(t *testing.T) {
	plans := &fakeGenericPlans{plans: map[string]string{}}
	tc := skipTable()
	tc.Queries = nil
	for i, calls := range []int64{10, 900, 50, 700, 300} {
		text := "SELECT id FROM t WHERE created_at = $1 LIMIT " + string(rune('1'+i))
		plans.plans[text] = skipScanPlanJSON
		tc.Queries = append(tc.Queries, QueryInfo{QueryID: int64(i + 1), Text: text,
			Calls: calls, MeanTimeMs: 1, TotalTimeMs: float64(calls)})
	}
	o, _, _ := skipOptimizer(newMemStore(), plans, verifiedResult)
	a := o.Admit(context.Background(), candidate(createdAtDDL), tc)
	want := []string{tc.Queries[1].Text, tc.Queries[3].Text, tc.Queries[4].Text}
	if len(plans.calls) != 1 || !reflect.DeepEqual(plans.calls[0], want) {
		t.Fatalf("EXPLAIN calls = %q, want the three most-called %q", plans.calls, want)
	}
	if a.Reason != "served_by_skip_scan:t_tenant_created" {
		t.Fatalf("admission %s %q", a.Outcome, a.Reason)
	}
}

// A statement that cannot be planned read-only leaves the evidence
// incomplete: no EXPLAIN, and the candidate proceeds.
func TestSkipScan_UnplannableStatementKeepsTheCandidate(t *testing.T) {
	plans := servedPlans()
	tc := skipTable()
	tc.Queries[0].Text = "UPDATE t SET note = $2 WHERE created_at = $1"
	o, w, _ := skipOptimizer(newMemStore(), plans, verifiedResult)
	a := o.Admit(context.Background(), candidate(createdAtDDL), tc)
	if a.Outcome != AdmitAccepted || w.calls.Load() != 1 || plans.callCount() != 0 {
		t.Fatalf("admission %s %q, what-ifs %d, EXPLAIN calls %d", a.Outcome, a.Reason,
			w.calls.Load(), plans.callCount())
	}
}

// PG 14-17: no EXPLAIN, and the admission is the one of an unknown
// version (byte-identical behaviour).
func TestSkipScan_BeforePG18NothingChanges(t *testing.T) {
	for _, major := range []int{14, 15, 16, 17} {
		plans := servedPlans()
		o, _, _ := skipOptimizer(newMemStore(), plans, verifiedResult)
		tc := skipTable()
		tc.PGMajor = major
		got := o.Admit(context.Background(), candidate(createdAtDDL), tc)
		base, _, _ := skipOptimizer(newMemStore(), nil, verifiedResult)
		tc.PGMajor = 0
		want := base.Admit(context.Background(), candidate(createdAtDDL), tc)
		if plans.callCount() != 0 || !reflect.DeepEqual(got, want) ||
			got.Outcome != AdmitAccepted {
			t.Errorf("PG %d: admission %+v (EXPLAIN calls %d), want %+v", major, got,
				plans.callCount(), want)
		}
	}
}

// The floor's candidates do not depend on the version: dropping happens
// at admission, on evidence.
func TestSkipScan_FloorCandidatesIgnoreTheVersion(t *testing.T) {
	tc := skipTable()
	tc.ColStats[1].NDistinct = 5000
	want := floorCandidates(tc)
	if len(want) != 1 || !strings.Contains(want[0].DDL, "(created_at)") {
		t.Fatalf("floor candidates = %+v, want one on created_at", want)
	}
	for _, major := range []int{0, 14, 17, 18} {
		tc.PGMajor = major
		if got := floorCandidates(tc); !reflect.DeepEqual(got, want) {
			t.Errorf("PG %d: floor candidates %+v, want %+v", major, got, want)
		}
	}
}

// HypoPG path: a low-improvement rejection of a skip-scan candidate names
// the serving index; before 18 the reason is unchanged.
func TestSkipScan_WhatIfRejectionNamesTheServingIndex(t *testing.T) {
	low := WhatIfResult{Measured: 1, Improvement: 1.5, SizeBytes: 8192}
	plans := &fakeGenericPlans{plans: map[string]string{}} // no plan shows the skip scan
	o, w, _ := skipOptimizer(newMemStore(), plans, low)
	a := o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	want := "call-weighted improvement 1.5% is below the 10.0% minimum; " +
		"already served by t_tenant_created through skip scan"
	if a.Outcome != AdmitRejected || a.Reason != want || w.calls.Load() != 1 {
		t.Fatalf("admission %s %q (what-ifs %d), want %q", a.Outcome, a.Reason,
			w.calls.Load(), want)
	}
	o17, _, _ := skipOptimizer(newMemStore(), nil, low)
	tc := skipTable()
	tc.PGMajor = 17
	a = o17.Admit(context.Background(), candidate(createdAtDDL), tc)
	if a.Reason != "call-weighted improvement 1.5% is below the 10.0% minimum" {
		t.Fatalf("PG 17 reason = %q", a.Reason)
	}
}

// State transition: a served_by_skip_scan rejection is remembered (no
// second EXPLAIN); once the composite is dropped, the candidate is judged
// again and proposed.
func TestSkipScan_RememberedUntilTheServingIndexIsDropped(t *testing.T) {
	store, plans := newMemStore(), servedPlans()
	o, w, logs := skipOptimizer(store, plans, verifiedResult)
	first := o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	if first.Outcome != AdmitRejected {
		t.Fatalf("first cycle = %s %q", first.Outcome, first.Reason)
	}
	second := o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	if second.Outcome != AdmitMeasured ||
		!strings.Contains(second.Reason, "served_by_skip_scan:t_tenant_created") ||
		plans.callCount() != 1 || w.calls.Load() != 0 {
		t.Fatalf("second cycle = %s %q (EXPLAIN calls %d, what-ifs %d)", second.Outcome,
			second.Reason, plans.callCount(), w.calls.Load())
	}
	dropped := skipTable()
	dropped.Indexes, dropped.IndexCount = nil, 0
	third := o.Admit(context.Background(), candidate(createdAtDDL), dropped)
	if third.Outcome != AdmitAccepted || w.calls.Load() != 1 || plans.callCount() != 1 {
		t.Fatalf("after the drop = %s %q (what-ifs %d, EXPLAIN calls %d)", third.Outcome,
			third.Reason, w.calls.Load(), plans.callCount())
	}
	if len(logs.matching("t_tenant_created", "judging it again")) != 1 {
		t.Fatalf("logs = %q, want one line saying the candidate is judged again", logs.lines)
	}
}

// The agent's packet line for a skip-scan rejection names the index, not
// an improvement percentage.
func TestSkipScan_PromptLineNamesTheServingIndex(t *testing.T) {
	store, plans := newMemStore(), servedPlans()
	o, _, _ := skipOptimizer(store, plans, verifiedResult)
	o.Admit(context.Background(), candidate(createdAtDDL), skipTable())
	lines := o.MeasuredRejections(context.Background(), skipTable())
	if len(lines) != 1 || !strings.Contains(lines[0], "already served by t_tenant_created "+
		"through PostgreSQL 18 skip scan") || strings.Contains(lines[0], "improvement") {
		t.Fatalf("prompt lines = %q", lines)
	}
}
