package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/planhash"
)

// indexes-07: a replacement was judged with the old index still present
// and a covered drop was never measured. The guard compares each guarded
// statement's cost before (old index present) and after (old index
// hidden).

// No concurrent access tests here: these helpers are pure functions of
// their arguments (the database tests cover parallel sessions).

func guardOf(ids ...int64) measurement {
	m := measurement{costs: map[int64]float64{}, uses: map[int64]bool{}}
	for _, id := range ids {
		m.costs[id] = 100
		m.uses[id] = true
	}
	return m
}

// Boundary: a guarded statement may rise 10.00%; 10.01% rejects.
func TestGuardedRegressionsBoundary(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1}, {QueryID: 2}, {QueryID: 3}}
	before := guardOf(1, 2, 3)
	after := map[int64]float64{1: 110, 2: 110.01, 3: 50}
	guarded, regs := guardedRegressions(queries, before, after)
	if guarded != 3 {
		t.Fatalf("guarded = %d, want 3", guarded)
	}
	if len(regs) != 1 || regs[0].QueryID != 2 {
		t.Fatalf("regressions = %+v, want only queryid 2 (10.01%%)", regs)
	}
	if regs[0].Pct < 10.009 || regs[0].Pct > 10.011 {
		t.Fatalf("regression pct = %v, want 10.01", regs[0].Pct)
	}
}

// Only statements whose baseline plan used the old index are guarded: an
// unguarded statement that gets costlier is the improvement's business.
func TestGuardedRegressionsIgnoreUnguardedStatements(t *testing.T) {
	queries := []QueryInfo{{QueryID: 1}, {QueryID: 2}}
	before := guardOf(1)
	before.costs[2] = 100
	after := map[int64]float64{1: 100, 2: 500}
	guarded, regs := guardedRegressions(queries, before, after)
	if guarded != 1 || len(regs) != 0 {
		t.Fatalf("guarded %d regressions %+v, want 1 guarded and none", guarded, regs)
	}
}

// Nil/empty: no guarded statements, a guarded statement not replanned, a
// duplicate queryid and a zero cost give no regression and no panic.
func TestGuardedRegressionsEmpty(t *testing.T) {
	if g, regs := guardedRegressions(nil, measurement{}, nil); g != 0 || regs != nil {
		t.Fatalf("empty = %d %+v", g, regs)
	}
	queries := []QueryInfo{{QueryID: 1}, {QueryID: 1}, {QueryID: 2}}
	before := guardOf(1, 2)
	before.costs[2] = 0
	g, regs := guardedRegressions(queries, before, map[int64]float64{2: 900})
	if g != 2 || len(regs) != 0 {
		t.Fatalf("guarded %d regressions %+v, want 2 guarded (dedup), none measured", g, regs)
	}
}

func TestPlanUsesIndexWalksNestedPlans(t *testing.T) {
	plan := []byte(`[{"Plan":{"Node Type":"Nested Loop","Plans":[
		{"Node Type":"Seq Scan"},
		{"Node Type":"Bitmap Heap Scan","Plans":[
			{"Node Type":"Bitmap Index Scan","Index Name":"orders_status_idx"}]}]}}]`)
	if !planhash.UsesIndex(plan, "orders_status_idx") {
		t.Fatal("nested bitmap index scan not found")
	}
	if planhash.UsesIndex(plan, "orders_status") || planhash.UsesIndex(plan, "") {
		t.Fatal("a prefix or an empty name matched")
	}
	if planhash.UsesIndex([]byte("not json"), "orders_status_idx") {
		t.Fatal("malformed plan reported a use")
	}
}

// A measured regression of a guarded statement rejects the replacement and
// names the queryid, before the improvement is looked at.
func TestWhatIfVerdictRejectsGuardedRegression(t *testing.T) {
	res := WhatIfResult{Improvement: 60, Measured: 3, SizeBytes: 8192, Guarded: 2,
		HiddenIndex: "items_a", Regressions: []QueryRegression{{QueryID: 77, Pct: 54.2}}}
	verdict, reason := whatIfVerdict(res, nil, 10)
	if verdict != WhatIfRejected {
		t.Fatalf("verdict = %s, want rejected", verdict)
	}
	for _, want := range []string{"items_a", "queryid 77", "54.2%", "10%"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	// Even a partial measurement: nothing unplanned undoes a measured regression.
	res.Failed, res.UnplannedQueryIDs = 1, []int64{5}
	if verdict, _ := whatIfVerdict(res, nil, 10); verdict != WhatIfRejected {
		t.Fatalf("partial measurement with a regression = %s, want rejected", verdict)
	}
}

// Nil/empty: no guarded statement gives the improvement-only verdict.
func TestWhatIfVerdictWithoutGuardedStatementsIsImprovementOnly(t *testing.T) {
	res := WhatIfResult{Improvement: 30, Measured: 2, SizeBytes: 8192,
		HiddenIndex: "items_a"}
	if verdict, reason := whatIfVerdict(res, nil, 10); verdict != WhatIfVerified ||
		reason != "" {
		t.Fatalf("verdict %s %q, want verified", verdict, reason)
	}
	res.Improvement = 5
	if verdict, _ := whatIfVerdict(res, nil, 10); verdict != WhatIfRejected {
		t.Fatalf("low improvement verdict = %s, want rejected", verdict)
	}
}

// Without hypopg_hide_index today's measurement stands, and the reason says
// it was taken with the old index present.
func TestWhatIfVerdictNamesMissingHideSupport(t *testing.T) {
	res := WhatIfResult{Improvement: 30, Measured: 2, SizeBytes: 8192, OldIndexPresent: true}
	verdict, reason := whatIfVerdict(res, nil, 10)
	if verdict != WhatIfVerified || reason != hideUnsupportedReason {
		t.Fatalf("verdict %s %q, want verified with %q", verdict, reason,
			hideUnsupportedReason)
	}
	res.Improvement = 1
	verdict, reason = whatIfVerdict(res, nil, 10)
	if verdict != WhatIfRejected || !strings.Contains(reason, "below the 10.0% minimum") ||
		!strings.Contains(reason, hideUnsupportedReason) {
		t.Fatalf("verdict %s %q, want rejected with both reasons", verdict, reason)
	}
}

func TestDropVerdict(t *testing.T) {
	ok := WhatIfResult{Improvement: -4, Measured: 3, Guarded: 1, HiddenIndex: "items_a"}
	regressed9 := []QueryRegression{{QueryID: 9, Pct: 40}}
	for name, tt := range map[string]struct {
		res     WhatIfResult
		err     error
		verdict string
		reason  string
	}{
		"verified": {ok, nil, WhatIfVerified, ""},
		"error":    {ok, errors.New("boom"), WhatIfUnverified, "boom"},
		"no hide":  {WhatIfResult{OldIndexPresent: true}, nil, WhatIfUnverified, "1.4+"},
		"nothing":  {WhatIfResult{}, nil, WhatIfUnverified, "no workload query"},
		"partial": {WhatIfResult{Measured: 1, Failed: 1}, nil, WhatIfUnverified,
			"not planned"},
		"regression": {WhatIfResult{Measured: 1, Guarded: 1, HiddenIndex: "items_a",
			Regressions: regressed9}, nil, WhatIfRejected, "queryid 9"},
		"no guarded": {WhatIfResult{Measured: 2}, nil, WhatIfVerified, ""},
		"partial+regr": {WhatIfResult{Measured: 1, Failed: 1, Regressions: regressed9},
			nil, WhatIfRejected, "queryid 9"},
	} {
		t.Run(name, func(t *testing.T) {
			verdict, reason := dropVerdict(tt.res, tt.err)
			if verdict != tt.verdict || !strings.Contains(reason, tt.reason) ||
				(tt.reason == "" && reason != "") {
				t.Fatalf("dropVerdict = %s %q, want %s containing %q", verdict, reason,
					tt.verdict, tt.reason)
			}
		})
	}
}

// fakeDropWhatIf is a what-if that can also measure drops.
type fakeDropWhatIf struct {
	available bool
	res       WhatIfResult
	err       error
	got       []IndexRef
	queries   int
}

func (f *fakeDropWhatIf) IsAvailable(context.Context) bool { return f.available }

func (f *fakeDropWhatIf) Validate(context.Context, Recommendation, []QueryInfo,
) (WhatIfResult, error) {
	return WhatIfResult{}, errors.New("not a create")
}

func (f *fakeDropWhatIf) MeasureDrop(_ context.Context, ix IndexRef, q []QueryInfo,
) (WhatIfResult, error) {
	f.got = append(f.got, ix)
	f.queries = len(q)
	return f.res, f.err
}

func dropOptimizer(w whatIfValidator) *Optimizer {
	o := New(nil, admissionConfig(), noopLog2)
	o.whatIf = w
	return o
}

// The measured change is the workload's cost change with the index hidden:
// a 4% call-weighted cost increase is +4 (slower), and 0 stays a plain 0.
func TestOptimizerMeasureDropVerified(t *testing.T) {
	w := &fakeDropWhatIf{available: true, res: WhatIfResult{Improvement: -4, Measured: 2,
		Guarded: 1, HiddenIndex: "items_a"}}
	tc := TableContext{Queries: []QueryInfo{{QueryID: 1}, {QueryID: 2}}}
	m := dropOptimizer(w).MeasureDrop(t.Context(), IndexRef{Name: "items_a", OID: 42}, tc)
	if m.Verdict != WhatIfVerified || m.ChangePct != 4 || m.Measured != 2 || m.Guarded != 1 {
		t.Fatalf("measurement = %+v, want verified +4%%", m)
	}
	if len(w.got) != 1 || w.got[0].OID != 42 || w.queries != 2 {
		t.Fatalf("what-if got %+v with %d queries", w.got, w.queries)
	}
	w.res = WhatIfResult{Measured: 1}
	m = dropOptimizer(w).MeasureDrop(t.Context(), IndexRef{Name: "items_a", OID: 42}, tc)
	if m.Verdict != WhatIfVerified || m.ChangePct != 0 || 1/m.ChangePct < 0 {
		t.Fatalf("zero change = %+v, want a plain 0", m)
	}
}

func TestOptimizerMeasureDropRejectsRegression(t *testing.T) {
	w := &fakeDropWhatIf{available: true, res: WhatIfResult{Improvement: -30, Measured: 1,
		Guarded: 1, HiddenIndex: "items_a",
		Regressions: []QueryRegression{{QueryID: 9, Pct: 30}}}}
	m := dropOptimizer(w).MeasureDrop(t.Context(), IndexRef{Name: "items_a", OID: 42},
		TableContext{Queries: []QueryInfo{{QueryID: 9}}})
	if m.Verdict != WhatIfRejected || !strings.Contains(m.Reason, "queryid 9") ||
		len(m.Regressions) != 1 || m.ChangePct != 0 {
		t.Fatalf("measurement = %+v, want rejected naming queryid 9", m)
	}
}

// Fail closed: unavailable HypoPG, a what-if that cannot measure drops, an
// unknown OID and an error are all unverified; none is measured.
func TestOptimizerMeasureDropUnverified(t *testing.T) {
	ix := IndexRef{Name: "items_a", OID: 42}
	for name, tt := range map[string]struct {
		w      whatIfValidator
		ix     IndexRef
		reason string
	}{
		"nil what-if": {nil, ix, "HypoPG unavailable"},
		"unavailable": {&fakeDropWhatIf{}, ix, "HypoPG unavailable"},
		"no drop":     {unavailableWhatIf{}, ix, "HypoPG unavailable"},
		"no oid":      {&fakeDropWhatIf{available: true}, IndexRef{Name: "items_a"}, "OID"},
		"error": {&fakeDropWhatIf{available: true, err: errors.New("conn lost")}, ix,
			"conn lost"},
		"no hide": {&fakeDropWhatIf{available: true,
			res: WhatIfResult{OldIndexPresent: true}}, ix, "hypopg_hide_index"},
		"nothing usable": {&fakeDropWhatIf{available: true}, ix, "no workload query"},
	} {
		t.Run(name, func(t *testing.T) {
			m := dropOptimizer(tt.w).MeasureDrop(t.Context(), tt.ix, TableContext{})
			if m.Verdict != WhatIfUnverified || !strings.Contains(m.Reason, tt.reason) ||
				m.ChangePct != 0 {
				t.Fatalf("measurement = %+v, want unverified containing %q", m, tt.reason)
			}
		})
	}
}

// Invalid input: an old index with no identity (OID 0) cannot be hidden.
// It is an error naming the index, before any SQL runs (the session has
// no transaction), not a lookup that reports OID 0 as gone.
func TestIndexToHideRejectsUnknownOID(t *testing.T) {
	s := &hypopgSession{canHide: true}
	old, err := s.indexToHide(context.Background(), &IndexRef{Name: "items_a"})
	if err == nil || !strings.Contains(err.Error(), "identity (OID) of index items_a") {
		t.Fatalf("indexToHide = %+v, %v; want the unknown identity named", old, err)
	}
	if old != (hiddenIndex{}) {
		t.Fatalf("hidden index = %+v, want none", old)
	}
}
