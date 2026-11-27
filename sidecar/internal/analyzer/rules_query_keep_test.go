package analyzer

import (
	"context"
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-05 review: the interval rules judge only a measured interval (a
// valid one with at least the call floor). A statement without one this
// cycle has no verdict, so its open slow_query or query_regression finding
// is kept, not resolved because the last minute happened to be idle.

// No concurrent access tests: the helpers are pure functions over two
// snapshots, and the keep set belongs to the single analyzer goroutine.

// keepPair is the fixture: every statement had 1,000 calls before.
//
//	1 idle in the interval, 2 below the floor (3 calls), 3 fast with 10
//	calls (judged), 4 slow with 10 calls (emitted), 5 one ad hoc call in
//	total, 6 new to the list, 7 evicted and re-entered (counters fell).
func keepPair() (*collector.Snapshot, *collector.Snapshot) {
	return intervalPair(60,
		[]collector.QueryStats{intervalStats(1, 1000, 1000), intervalStats(2, 1000, 1000),
			intervalStats(3, 1000, 1000), intervalStats(4, 1000, 1000),
			intervalStats(7, 1000, 1000)},
		[]collector.QueryStats{intervalStats(1, 1000, 1000), intervalStats(2, 1003, 1003),
			intervalStats(3, 1010, 1010), intervalStats(4, 1010, 1000+10*5000),
			intervalStats(5, 1, 6000), intervalStats(6, 50, 50), intervalStats(7, 20, 20)})
}

func sortedIdents(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantIdents(t *testing.T, what string, got map[string]bool, want ...string) {
	t.Helper()
	g := sortedIdents(got)
	if len(g) != len(want) {
		t.Fatalf("%s = %v, want %v", what, g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, g, want)
		}
	}
}

// Happy path and boundaries: idle, thin, new and evicted statements are
// undecided; a fast or slow measured interval is decided; one that never
// reached the floor in total is not kept.
func TestUndecidedQueries_OnlyStatementsWithoutAMeasuredInterval(t *testing.T) {
	cur, prev := keepPair()
	got := map[string]bool{}
	for _, id := range undecidedQueries(cur, prev, 5) {
		got[id] = true
	}
	wantIdents(t, "undecided", got, "queryid:1", "queryid:2", "queryid:6", "queryid:7")
	// Boundary: a floor of 3 measures statement 2's 3 calls.
	got = map[string]bool{}
	for _, id := range undecidedQueries(cur, prev, 3) {
		got[id] = true
	}
	wantIdents(t, "undecided at floor 3", got, "queryid:1", "queryid:6", "queryid:7")
}

// Nil/empty: no statements, nothing kept.
func TestUndecidedQueries_Empty(t *testing.T) {
	cur, prev := intervalPair(60, nil, nil)
	if got := undecidedQueries(cur, prev, 5); len(got) != 0 {
		t.Fatalf("undecided = %v, want none", got)
	}
}

// State transition: the cycle keeps the undecided slow_query findings
// open and leaves the decided ones resolvable.
func TestRunSnapshotRules_KeepsUndecidedSlowQueries(t *testing.T) {
	a := New(nil, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur, prev := keepPair()
	out := a.runSnapshotRules(cur, prev, false)
	var slow []string
	for _, f := range out {
		if f.Category == "slow_query" {
			slow = append(slow, f.ObjectIdentifier)
		}
	}
	if len(slow) != 1 || slow[0] != "queryid:4" {
		t.Fatalf("slow_query findings = %v, want queryid:4", slow)
	}
	if !a.eval.resolvable(out)["slow_query"] {
		t.Fatal("slow_query not resolvable on a measured interval")
	}
	wantIdents(t, "kept slow_query", a.eval.kept("slow_query"),
		"queryid:1", "queryid:2", "queryid:6", "queryid:7")
}

// Without a measured interval (no previous snapshot) the category is not
// evaluated, and nothing is kept by identity.
func TestRunSnapshotRules_NoIntervalKeepsNothing(t *testing.T) {
	a := New(nil, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur, _ := keepPair()
	out := a.runSnapshotRules(cur, nil, false)
	if a.eval.resolvable(out)["slow_query"] {
		t.Fatal("slow_query resolvable without a previous snapshot")
	}
	if got := a.eval.kept("slow_query"); len(got) != 0 {
		t.Fatalf("kept = %v, want none", got)
	}
}

// query_regression: an undecided statement, a candidate without a baseline
// and a candidate past the cap are kept; a candidate judged against its
// baseline and a measured statement within the threshold are not.
func TestUnjudgedRegressions(t *testing.T) {
	prevQ := []collector.QueryStats{intervalStats(1, 1000, 1000), intervalStats(3, 1000, 1000)}
	curQ := []collector.QueryStats{intervalStats(1, 1000, 1000), intervalStats(3, 1010, 1010)}
	for i := int64(10); i < 10+maxRegressionCandidates+2; i++ {
		prevQ = append(prevQ, intervalStats(i, 1000, 1000))
		curQ = append(curQ, intervalStats(i, 1100, 1000+100*(10+float64(i))))
	}
	cur, prev := intervalPair(60, prevQ, curQ)
	cfg := intervalConfig()
	cands := regressionCandidates(cur, prev, cfg)
	if len(cands) != maxRegressionCandidates || cands[0].q.QueryID != 61 {
		t.Fatalf("candidates = %d, first %d; want 50 from query 61", len(cands),
			cands[0].q.QueryID)
	}
	baselines := map[int64]regressionBaseline{}
	for _, c := range cands {
		if c.q.QueryID != 61 {
			baselines[c.q.QueryID] = regressionBaseline{MeanMs: 2, Buckets: 24}
		}
	}
	baselines[12] = regressionBaseline{MeanMs: 0, Buckets: 24}
	got := map[string]bool{}
	for _, id := range unjudgedRegressions(cur, prev, cfg, cands, baselines) {
		got[id] = true
	}
	// 1 idle; 61 no baseline; 12 a zero baseline (no verdict); 10 and 11
	// past the cap of 50 (61..12 are the candidates). 3 is within the
	// threshold of its cumulative mean, the rest are judged.
	wantIdents(t, "unjudged", got, "queryid:1", "queryid:10", "queryid:11", "queryid:12",
		"queryid:61")
}

// The cycle keeps the unjudged regressions: with no candidate it reads
// nothing and still keeps the idle statement open.
func TestRunQueryRegression_KeepsUnjudgedWithoutARead(t *testing.T) {
	a := New(nil, intervalConfig(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	cur, prev := keepPair()
	// Without statement 4 (1 ms -> 5,000 ms, a candidate) there is no
	// candidate, so the nil pool is never read.
	cur.Queries = append(cur.Queries[:3:3], cur.Queries[4:]...)
	if got := a.runQueryRegression(context.Background(), cur, prev); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
	if a.eval.hasFailed("query_regression") {
		t.Fatal("query_regression failed without a read")
	}
	wantIdents(t, "kept query_regression", a.eval.kept("query_regression"),
		"queryid:1", "queryid:2", "queryid:6", "queryid:7")
}

// Candidates obey the call floor on the current interval too: a 4-call
// interval far above its cumulative mean is not a candidate, 5 calls is.
func TestRegressionCandidates_CallFloor(t *testing.T) {
	for _, tc := range []struct {
		calls int64
		want  int
	}{{4, 0}, {5, 1}} {
		cur, prev := intervalPair(60,
			[]collector.QueryStats{intervalStats(1, 1000, 1000)},
			[]collector.QueryStats{intervalStats(1, 1000+tc.calls,
				1000+float64(tc.calls)*500)})
		if got := regressionCandidates(cur, prev, intervalConfig()); len(got) != tc.want {
			t.Fatalf("%d calls: %d candidates, want %d", tc.calls, len(got), tc.want)
		}
	}
}
