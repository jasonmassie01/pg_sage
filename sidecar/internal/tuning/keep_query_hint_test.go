package tuning

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// tuning-06: an open query_tuning finding resolves when its statement left
// pg_stat_statements for N cycles, stayed below the query tuner's admission
// thresholds for N cycles, or its hint left the proposed state. Anything
// unknown (unreadable statistics, a queryid not reported, an interval the
// snapshots cannot measure) keeps the finding exactly as it was.

const qhID = int64(4_242_001)

func queryHintFinding(qid int64) analyzer.Finding {
	return analyzer.Finding{Category: "query_tuning", Severity: "warning",
		ObjectType: "query", ObjectIdentifier: "queryid:" + strconv.FormatInt(qid, 10),
		Title: "Plan hint", Detail: map[string]any{"queryid": qid,
			"hint_directive": "HashJoin(a b)"},
		RecommendedSQL: "INSERT INTO hint_plan.hints ...", ActionRisk: "safe"}
}

// stmtSnap is a snapshot holding one statement's cumulative counters.
func stmtSnap(qid, calls int64, totalMs float64, temp int64) *collector.Snapshot {
	return &collector.Snapshot{Queries: []collector.QueryStats{{QueryID: qid,
		Calls: calls, TotalExecTime: totalMs, TempBlksWritten: temp}}}
}

// keepCycles runs keepOpen n times, feeding each cycle's kept findings back
// as the next cycle's open findings (the analyzer's upsert), and returns
// the kept findings of every cycle.
func keepCycles(h *harness, open []analyzer.Finding, n int, cur, prev *collector.Snapshot,
) [][]analyzer.Finding {
	var outs [][]analyzer.Finding
	for i := 0; i < n; i++ {
		kept, _ := h.agent.keepOpen(context.Background(), cur, prev, open, nil)
		outs = append(outs, kept)
		open = kept
	}
	return outs
}

func detailCount(f analyzer.Finding, key string) int {
	n, _ := cycleCount(f.Detail[key])
	return n
}

// State transition / boundary: absent for N-1 cycles stays open with the
// count recorded on the finding; the Nth absent cycle resolves it.
func TestQueryHint_AbsentQueryidResolvesAtN(t *testing.T) {
	h := newHarness(t)
	h.store.qstates = map[int64]QueryState{qhID: {Present: false}}
	outs := keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, 3, nil, nil)
	for i, want := range []int{1, 2} {
		f, ok := byIdent(outs[i], "queryid:4242001")
		if !ok {
			t.Fatalf("cycle %d: absent for %d cycles resolved before N=3", i+1, want)
		}
		if got := detailCount(f, "absent_cycles"); got != want {
			t.Fatalf("cycle %d: absent_cycles = %d, want %d", i+1, got, want)
		}
	}
	if _, ok := byIdent(outs[2], "queryid:4242001"); ok {
		t.Fatal("absent for 3 cycles (N=3) is still open")
	}
	if !h.logs.contains("absent from pg_stat_statements for 3 cycles") {
		t.Fatalf("the resolve reason is not logged: %v", h.logs.lines)
	}
	if len(h.store.qAsked) != 3 || !reflect.DeepEqual(h.store.qAsked[0], []int64{qhID}) {
		t.Fatalf("QueryStates asked %v, want [%d] once per cycle", h.store.qAsked, qhID)
	}
}

// State transition: a statement that reappears resets the absence count.
func TestQueryHint_ReappearingQueryidResetsTheCount(t *testing.T) {
	h := newHarness(t)
	h.store.qstates = map[int64]QueryState{qhID: {Present: false}}
	outs := keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, 2, nil, nil)
	h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true}}
	back := keepCycles(h, outs[1], 1, nil, nil)
	f, ok := byIdent(back[0], "queryid:4242001")
	if !ok {
		t.Fatal("a present, still-slow statement resolved")
	}
	if _, has := f.Detail["absent_cycles"]; has {
		t.Fatalf("present again, the absence count stays: %v", f.Detail)
	}
	h.store.qstates = map[int64]QueryState{qhID: {Present: false}}
	again := keepCycles(h, back[0], 2, nil, nil)
	if _, ok := byIdent(again[1], "queryid:4242001"); !ok {
		t.Fatal("absent 2 cycles after a reset resolved: the count was not reset")
	}
}

// Boundary: tuner.hint_absent_cycles sets N; zero or less means 3.
func TestQueryHint_AbsentCyclesSetting(t *testing.T) {
	for _, tc := range []struct{ setting, resolvesAt int }{{1, 1}, {2, 2}, {0, 3}, {-4, 3}} {
		s := defaultSettings()
		s.HintAbsentCycles = tc.setting
		h := newHarnessWith(t, s)
		h.store.qstates = map[int64]QueryState{qhID: {Present: false}}
		outs := keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, tc.resolvesAt,
			nil, nil)
		for i := 0; i < tc.resolvesAt-1; i++ {
			if _, ok := byIdent(outs[i], "queryid:4242001"); !ok {
				t.Fatalf("setting %d: resolved at cycle %d, want %d", tc.setting, i+1,
					tc.resolvesAt)
			}
		}
		if _, ok := byIdent(outs[tc.resolvesAt-1], "queryid:4242001"); ok {
			t.Fatalf("setting %d: open after %d absent cycles", tc.setting, tc.resolvesAt)
		}
	}
}

// State transitions: a hint that left the proposed state resolves its
// finding at once; a proposed hint, or none, keeps it.
func TestQueryHint_HintStatusResolves(t *testing.T) {
	for _, status := range []string{"active", "dormant", "broken", "retired",
		"rolled_back", "rejected", "expired"} {
		h := newHarness(t)
		h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true,
			HintStatus: status}}
		outs := keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, 1, nil, nil)
		if _, ok := byIdent(outs[0], "queryid:4242001"); ok {
			t.Fatalf("hint %s: the finding stays open", status)
		}
		if !h.logs.contains("hint is " + status) {
			t.Fatalf("hint %s: the reason is not logged: %v", status, h.logs.lines)
		}
	}
	for _, status := range []string{"proposed", ""} {
		h := newHarness(t)
		h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true,
			HintStatus: status}}
		open := queryHintFinding(qhID)
		outs := keepCycles(h, []analyzer.Finding{open}, 1, nil, nil)
		if got, ok := byIdent(outs[0], "queryid:4242001"); !ok ||
			!reflect.DeepEqual(got, open) {
			t.Fatalf("hint %q: want the finding kept unchanged, got %+v", status, outs[0])
		}
	}
}

// Boundary: below the tuner's admission thresholds for N-1 interval cycles
// stays open; the Nth resolves. 100 ms is the tuner's own bound (it admits
// a mean above it).
func TestQueryHint_QuietForNCyclesResolves(t *testing.T) {
	h := newHarness(t)
	h.store.qstates = map[int64]QueryState{qhID: {Present: true}}
	prev, cur := stmtSnap(qhID, 1000, 50_000, 0), stmtSnap(qhID, 1010, 51_000, 0)
	outs := keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, 3, cur, prev)
	for i := 0; i < 2; i++ {
		f, ok := byIdent(outs[i], "queryid:4242001")
		if !ok || detailCount(f, "quiet_cycles") != i+1 {
			t.Fatalf("cycle %d: want open with quiet_cycles=%d, got %+v", i+1, i+1, outs[i])
		}
	}
	if _, ok := byIdent(outs[2], "queryid:4242001"); ok {
		t.Fatal("quiet (interval mean exactly 100 ms) for 3 cycles is still open")
	}
	if !h.logs.contains("below the query tuner's admission thresholds for 3 cycles") {
		t.Fatalf("the resolve reason is not logged: %v", h.logs.lines)
	}
}

// State transitions of the quiet count: a slow interval, temp spills or a
// statement the tuner would admit now reset it; an interval the snapshots
// cannot measure leaves it as it was.
func TestQueryHint_QuietCountResetAndUnknown(t *testing.T) {
	cases := []struct {
		name       string
		state      QueryState
		prev, cur  *collector.Snapshot
		wantQuiet  int
		wantSeeded bool
	}{
		{"slow interval", QueryState{Present: true}, stmtSnap(qhID, 1000, 0, 0),
			stmtSnap(qhID, 1010, 1005, 0), 0, false},
		{"temp spill", QueryState{Present: true}, stmtSnap(qhID, 1000, 0, 5),
			stmtSnap(qhID, 1010, 10, 6), 0, false},
		{"tuner admits it", QueryState{Present: true, Admitted: true},
			stmtSnap(qhID, 1000, 0, 0), stmtSnap(qhID, 1010, 10, 0), 0, false},
		{"not in the snapshots", QueryState{Present: true}, nil, stmtSnap(7, 1, 1, 0),
			2, true},
		{"no previous snapshot", QueryState{Present: true}, nil,
			stmtSnap(qhID, 1010, 10, 0), 2, true},
		{"no calls this interval", QueryState{Present: true},
			stmtSnap(qhID, 1000, 5000, 0), stmtSnap(qhID, 1000, 5000, 0), 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.qstates = map[int64]QueryState{qhID: tc.state}
			open := queryHintFinding(qhID)
			open.Detail["quiet_cycles"] = float64(2) // as read back from JSON
			outs := keepCycles(h, []analyzer.Finding{open}, 1, tc.cur, tc.prev)
			f, ok := byIdent(outs[0], "queryid:4242001")
			if tc.wantQuiet >= 3 {
				if ok {
					t.Fatalf("quiet_cycles 2 -> 3 must resolve, kept %+v", f)
				}
				return
			}
			if !ok {
				t.Fatal("the finding resolved")
			}
			if got := detailCount(f, "quiet_cycles"); got != tc.wantQuiet {
				t.Fatalf("quiet_cycles = %d, want %d", got, tc.wantQuiet)
			}
			if _, has := f.Detail["quiet_cycles"]; has != tc.wantSeeded {
				t.Fatalf("quiet_cycles key present = %t, want %t", has, tc.wantSeeded)
			}
		})
	}
}

// Error propagation: unreadable statistics keep every query_tuning finding
// exactly as it was, and say so.
func TestQueryHint_UnreadableStatesKeepEverything(t *testing.T) {
	h := newHarness(t)
	h.store.qErr = errors.New("permission denied for view pg_stat_statements")
	open := queryHintFinding(qhID)
	open.Detail["absent_cycles"] = float64(2)
	outs := keepCycles(h, []analyzer.Finding{open}, 1, nil, nil)
	got, ok := byIdent(outs[0], "queryid:4242001")
	if !ok || !reflect.DeepEqual(got, open) {
		t.Fatalf("an unreadable pg_stat_statements changed the finding: %+v", outs[0])
	}
	if !h.logs.contains("permission denied for view pg_stat_statements") {
		t.Fatalf("the read error is not logged: %v", h.logs.lines)
	}
}

// Error propagation: a catalog error keeps index findings and does not
// stop the statement check; a statement error does not resolve index
// findings either.
func TestQueryHint_CatalogErrorKeepsHintFinding(t *testing.T) {
	h := newHarness(t)
	h.store.catErr = errors.New("relation catalog unreadable")
	h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true}}
	idx := verifiedMemoriesIndex()
	open := queryHintFinding(qhID)
	outs := keepCycles(h, []analyzer.Finding{idx, open}, 1, nil, nil)
	if _, ok := byIdent(outs[0], idx.ObjectIdentifier); !ok {
		t.Fatal("a catalog error resolved an index finding")
	}
	if got, ok := byIdent(outs[0], "queryid:4242001"); !ok || !reflect.DeepEqual(got, open) {
		t.Fatalf("a catalog error changed the hint finding: %+v", outs[0])
	}
}

// Nil/empty: no query_tuning findings read nothing; a finding without a
// parseable queryid, or one QueryStates does not report, is kept unchanged.
func TestQueryHint_NothingToAskOrUnknown(t *testing.T) {
	h := newHarness(t)
	keepCycles(h, []analyzer.Finding{verifiedMemoriesIndex()}, 1, nil, nil)
	keepCycles(h, nil, 1, nil, nil)
	if len(h.store.qAsked) != 0 {
		t.Fatalf("QueryStates read without a query_tuning finding: %v", h.store.qAsked)
	}
	bad := queryHintFinding(qhID)
	bad.ObjectIdentifier = "queryid:abc"
	none := queryHintFinding(qhID)
	none.ObjectIdentifier = ""
	unreported := queryHintFinding(9)
	h.store.qstates = map[int64]QueryState{} // reports nothing
	outs := keepCycles(h, []analyzer.Finding{bad, none, unreported}, 1, nil, nil)
	if len(outs[0]) != 3 {
		t.Fatalf("kept %d of 3 findings it cannot judge: %+v", len(outs[0]), outs[0])
	}
	for i, want := range []analyzer.Finding{bad, none, unreported} {
		if !reflect.DeepEqual(outs[0][i], want) {
			t.Fatalf("finding %d changed: %+v", i, outs[0][i])
		}
	}
	if len(h.store.qAsked) != 1 || !reflect.DeepEqual(h.store.qAsked[0], []int64{9}) {
		t.Fatalf("asked %v, want only the parseable queryid 9", h.store.qAsked)
	}
}

// The admission thresholds QueryStates is asked with are the tuner's.
func TestQueryHint_AsksWithTheTunerAdmission(t *testing.T) {
	s := defaultSettings()
	s.HintAdmission = HintAdmission{MinCalls: 25, PlanTimeRatio: 2.5}
	h := newHarnessWith(t, s)
	h.store.qstates = map[int64]QueryState{qhID: {Present: true, Admitted: true}}
	keepCycles(h, []analyzer.Finding{queryHintFinding(qhID)}, 1, nil, nil)
	if h.store.qAdm != s.HintAdmission {
		t.Fatalf("asked with %+v, want %+v", h.store.qAdm, s.HintAdmission)
	}
}

// Concurrent access: two cycles running at once over the same open finding
// each advance the count by exactly one and never change the finding they
// were given (the store's copy is shared).
func TestQueryHint_ConcurrentCyclesOneTransitionEach(t *testing.T) {
	h := newHarness(t)
	h.store.qstates = map[int64]QueryState{qhID: {Present: false}}
	open := queryHintFinding(qhID)
	open.Detail["absent_cycles"] = float64(1)
	h.store.open = []analyzer.Finding{open}
	prev, cur := ordersPair()
	var wg sync.WaitGroup
	outs := make([]analyzer.TuningOutput, 2)
	for i := range outs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], _ = h.agent.Tune(context.Background(), cur, prev)
		}(i)
	}
	wg.Wait()
	for i, out := range outs {
		f, ok := byIdent(out.Findings, "queryid:4242001")
		if !ok || detailCount(f, "absent_cycles") != 2 {
			t.Fatalf("cycle %d: want absent_cycles 1 -> 2 exactly once, got %+v", i,
				out.Findings)
		}
	}
	if got := detailCount(h.store.open[0], "absent_cycles"); got != 1 {
		t.Fatalf("the stored finding was changed in place: absent_cycles = %d", got)
	}
}

// busyCases: an open hint finding no longer holds its whole case; the
// tuner's one-hint-per-statement check refuses a second hint, while index
// and statistics answers for the case are asked for again.
func TestQueryHint_HintFindingDoesNotHoldItsCase(t *testing.T) {
	cases := []Case{{ID: "top_statement:201", Tables: []string{"public.orders"}},
		{ID: "top_statement:202", Tables: []string{"public.orders"}}}
	hint := queryHintFinding(201)
	hint.Detail["producer"], hint.Detail["case_id"] = Producer, "top_statement:201"
	index := agentFinding("top_statement:202", optimizer.OptimizerCategory,
		"public.orders|btree(status)")
	busy := busyCases([]analyzer.Finding{hint, index}, cases)
	if busy["top_statement:201"] {
		t.Fatal("an open hint finding holds its whole case")
	}
	if !busy["top_statement:202"] {
		t.Fatal("an open index finding no longer holds its case")
	}
}

func TestCycleCount(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{{float64(2), 2, true}, {3, 3, true}, {int64(4), 4, true}, {nil, 0, false},
		{"2", 0, false}, {float64(-1), 0, false}, {float64(2.5), 0, false}} {
		got, ok := cycleCount(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("cycleCount(%v) = %d, %t; want %d, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
