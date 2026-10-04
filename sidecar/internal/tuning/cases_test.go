package tuning

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Case detection (owner decision 1): a case is a workload problem — a top
// statement by call-weighted time, a regression, or write amplification —
// never a per-table prompt.

func detect(prev, cur *collector.Snapshot) []Case {
	return DetectCases(cur, prev, ClassifyWorkload(cur, nil, t0), DefaultThresholds())
}

func caseIDs(cs []Case) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func hasCase(cs []Case, id string) (Case, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return Case{}, false
}

// shareSnaps builds an interval in which statement 1 spent stmtMs of a
// workload total of totalMs (statement 2 spent the rest), both on
// public.orders, with calls calls each.
func shareSnaps(stmtMs, totalMs float64, calls int64) (prev, cur *collector.Snapshot) {
	tbl := []collector.TableStats{table("public", "orders", 1000, 0)}
	q1 := "SELECT * FROM public.orders WHERE a = $1"
	q2 := "SELECT * FROM public.orders WHERE b = $1"
	prev = snapAt(t0, []collector.QueryStats{stmt(1, q1, 100, 1000), stmt(2, q2, 100, 1000)},
		tbl, nil)
	cur = snapAt(t0.Add(5*time.Minute), []collector.QueryStats{
		stmt(1, q1, 100+calls, 1000+stmtMs), stmt(2, q2, 100+calls, 1000+totalMs-stmtMs)},
		tbl, nil)
	return prev, cur
}

func TestDetectCases_TopStatementByIntervalShare(t *testing.T) {
	prev, cur := ordersPair()
	cs := detect(prev, cur)
	c, ok := hasCase(cs, "top_statement:101")
	if !ok {
		t.Fatalf("cases %v: want top_statement:101", caseIDs(cs))
	}
	// Interval: 101 spent 6000 ms of 6100 ms.
	if math.Abs(c.Weight-6000.0/6100.0) > 1e-9 {
		t.Fatalf("weight = %v, want %v (interval share)", c.Weight, 6000.0/6100.0)
	}
	if len(c.Statements) != 1 || c.Statements[0].Calls != 600 ||
		c.Statements[0].TotalMs != 6000 || c.Statements[0].MeanMs != 10 {
		t.Fatalf("statement uses interval deltas: %+v", c.Statements)
	}
	if len(c.Tables) != 1 || c.Tables[0] != "public.orders" {
		t.Fatalf("tables = %v", c.Tables)
	}
	if _, ok := hasCase(cs, "top_statement:102"); ok {
		t.Fatal("102 spent 100 ms (1.6%), below the minimum share")
	}
	if !strings.Contains(c.Reason, "%") {
		t.Fatalf("reason must state the share: %q", c.Reason)
	}
}

func TestDetectCases_ShareAndTimeBoundaries(t *testing.T) {
	cases := []struct {
		name          string
		stmtMs, total float64
		want          bool
	}{
		{"share exactly 5%", 1000, 20000, true},
		{"share just below 5%", 999, 20000, false},
		{"time exactly 1000 ms", 1000, 1000, true},
		{"time just below 1000 ms", 999.9, 999.9, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, cur := shareSnaps(tc.stmtMs, tc.total, 50)
			_, got := hasCase(detect(prev, cur), "top_statement:1")
			if got != tc.want {
				t.Fatalf("case = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectCases_MinimumCalls(t *testing.T) {
	prev, cur := shareSnaps(5000, 6000, 9)
	if _, ok := hasCase(detect(prev, cur), "top_statement:1"); ok {
		t.Fatal("9 calls in the interval are below the minimum of 10")
	}
	prev, cur = shareSnaps(5000, 6000, 10)
	if _, ok := hasCase(detect(prev, cur), "top_statement:1"); !ok {
		t.Fatal("exactly 10 calls qualify")
	}
}

// regressionSnaps: statement 7's cumulative mean before the interval is
// 10 ms; in the interval it runs calls times at intervalMean.
func regressionSnaps(intervalMean float64, calls int64) (prev, cur *collector.Snapshot) {
	q := "SELECT * FROM public.orders WHERE status = $1"
	tbl := []collector.TableStats{table("public", "orders", 1000, 0)}
	prev = snapAt(t0, []collector.QueryStats{stmt(7, q, 1000, 10000)}, tbl, nil)
	cur = snapAt(t0.Add(5*time.Minute), []collector.QueryStats{
		stmt(7, q, 1000+calls, 10000+intervalMean*float64(calls))}, tbl, nil)
	return prev, cur
}

func TestDetectCases_RegressionAtTwiceThePriorMean(t *testing.T) {
	prev, cur := regressionSnaps(20, 100)
	cs := detect(prev, cur)
	c, ok := hasCase(cs, "regression:7")
	if !ok {
		t.Fatalf("mean 10 -> 20 ms is a regression: %v", caseIDs(cs))
	}
	if c.Statements[0].PrevMeanMs != 10 || c.Statements[0].MeanMs != 20 {
		t.Fatalf("regression statement: %+v", c.Statements[0])
	}
	if _, dup := hasCase(cs, "top_statement:7"); dup {
		t.Fatal("one statement makes one case: the regression wins")
	}
	prev, cur = regressionSnaps(19.9, 100)
	cs = detect(prev, cur)
	if _, ok := hasCase(cs, "regression:7"); ok {
		t.Fatal("1.99x is not a regression")
	}
	if _, ok := hasCase(cs, "top_statement:7"); !ok {
		t.Fatal("it is still the top statement")
	}
}

func TestDetectCases_FirstCycleAndResetUseCumulativeCounters(t *testing.T) {
	_, cur := ordersPair()
	cs := DetectCases(cur, nil, ClassifyWorkload(cur, nil, t0), DefaultThresholds())
	c, ok := hasCase(cs, "top_statement:101")
	if !ok || c.Statements[0].Calls != 1600 {
		t.Fatalf("without a previous snapshot cumulative counters are used: %+v", cs)
	}
	prev, cur2 := ordersPair()
	prev.Queries[0].Calls = 99999 // counters went backwards: a reset
	prev.Queries[0].TotalExecTime = 999999
	cs = detect(prev, cur2)
	c, ok = hasCase(cs, "top_statement:101")
	if !ok || c.Statements[0].Calls != 1600 {
		t.Fatalf("after a reset the cumulative counters are the interval: %+v", cs)
	}
	if _, ok := hasCase(cs, "regression:101"); ok {
		t.Fatal("a reset is never a regression")
	}
}

func TestDetectCases_NonWorkloadNeverBecomesACase(t *testing.T) {
	tbl := []collector.TableStats{table("public", "orders", 1000, 0),
		table("test_x", "orders", 10, 0)}
	q := "SELECT * FROM public.orders WHERE a = $1"
	prev := snapAt(t0, []collector.QueryStats{stmt(1, q, 100, 100),
		stmt(2, "EXPLAIN ANALYZE SELECT * FROM public.orders", 1, 1),
		stmt(3, "SELECT * FROM test_x.orders", 1, 1)}, tbl, nil)
	cur := snapAt(t0.Add(time.Minute), []collector.QueryStats{stmt(1, q, 200, 1300),
		stmt(2, "EXPLAIN ANALYZE SELECT * FROM public.orders", 50, 900001),
		stmt(3, "SELECT * FROM test_x.orders", 500, 800001)}, tbl, nil)
	cs := detect(prev, cur)
	for _, c := range cs {
		if strings.HasSuffix(c.ID, ":2") || strings.HasSuffix(c.ID, ":3") {
			t.Fatalf("diagnostic or test statement became case %s", c.ID)
		}
	}
	c, ok := hasCase(cs, "top_statement:1")
	if !ok || c.Weight != 1 {
		t.Fatalf("non-workload time is not in the total: %+v", cs)
	}
}

// writeSnaps: public.events gets ins inserts and upd updates (hot of them
// HOT) over secs seconds, with the given indexes.
func writeSnaps(ins, upd, hot int64, secs float64, idx []collector.IndexStats,
	live, dead int64) (prev, cur *collector.Snapshot) {
	before := table("public", "events", live, dead)
	after := before
	after.NTupIns, after.NTupUpd, after.NTupHotUpd = ins, upd, hot
	prev = snapAt(t0, nil, []collector.TableStats{before}, idx)
	cur = snapAt(t0.Add(time.Duration(secs*float64(time.Second))), nil,
		[]collector.TableStats{after}, idx)
	return prev, cur
}

func eventsIndexes(unusedScans int64) []collector.IndexStats {
	pk := index("public", "events", "events_pkey",
		"CREATE UNIQUE INDEX events_pkey ON public.events USING btree (id)", 0)
	pk.IsUnique, pk.IsPrimary = true, true
	return []collector.IndexStats{pk,
		index("public", "events", "events_kind_idx",
			"CREATE INDEX events_kind_idx ON public.events USING btree (kind)", unusedScans)}
}

func TestDetectCases_WriteAmplificationWithAnUnusedIndex(t *testing.T) {
	// 2 indexes, 15000 inserts in 300 s: 100 index writes/s.
	prev, cur := writeSnaps(15000, 0, 0, 300, eventsIndexes(0), 100000, 0)
	cs := detect(prev, cur)
	c, ok := hasCase(cs, "write_amplification:public.events")
	if !ok {
		t.Fatalf("cases %v: want write amplification", caseIDs(cs))
	}
	if c.Weight != 1 || !strings.Contains(c.Reason, "events_kind_idx") {
		t.Fatalf("case %+v: weight 1 and the unused index named", c)
	}
	// The unused unique primary key alone is not a reason.
	prev, cur = writeSnaps(15000, 0, 0, 300, eventsIndexes(5), 100000, 0)
	if _, ok := hasCase(detect(prev, cur), "write_amplification:public.events"); ok {
		t.Fatal("every non-unique index is scanned: no write case")
	}
}

func TestDetectCases_IndexWriteRateBoundary(t *testing.T) {
	// 2 indexes; (ins + upd - hot) * 2 / secs. 7500 ins in 300 s = 50/s.
	prev, cur := writeSnaps(7500, 0, 0, 300, eventsIndexes(0), 100000, 0)
	if _, ok := hasCase(detect(prev, cur), "write_amplification:public.events"); !ok {
		t.Fatal("exactly 50 index writes/s qualifies")
	}
	// HOT updates write no index entries: 7500 ins + 300 upd (all HOT).
	prev, cur = writeSnaps(7499, 300, 300, 300, eventsIndexes(0), 100000, 0)
	if _, ok := hasCase(detect(prev, cur), "write_amplification:public.events"); ok {
		t.Fatal("49.99 index writes/s (HOT updates excluded) does not qualify")
	}
}

func TestDetectCases_RedundantIndexCountsAsWriteAmplification(t *testing.T) {
	idx := []collector.IndexStats{
		index("public", "events", "events_kind_idx",
			"CREATE INDEX events_kind_idx ON public.events USING btree (kind)", 50),
		index("public", "events", "events_kind_at_idx",
			"CREATE INDEX events_kind_at_idx ON public.events USING btree (kind, at)", 70),
	}
	prev, cur := writeSnaps(15000, 0, 0, 300, idx, 100000, 0)
	c, ok := hasCase(detect(prev, cur), "write_amplification:public.events")
	if !ok || !strings.Contains(c.Reason, "events_kind_idx") {
		t.Fatalf("(kind) is covered by (kind, at): %+v", c)
	}
}

func TestDetectCases_DeadTupleChurnBoundaries(t *testing.T) {
	one := []collector.IndexStats{index("public", "events", "events_kind_idx",
		"CREATE INDEX events_kind_idx ON public.events USING btree (kind)", 5)}
	cases := []struct {
		name       string
		live, dead int64
		bytes      int64
		upd        int64
		want       bool
	}{
		{"at every floor", 4000, 1000, 8 << 20, 10, true},
		{"dead below 1000", 3996, 999, 8 << 20, 10, false},
		{"ratio below 20%", 4001, 1000, 8 << 20, 10, false},
		{"table below 8 MiB", 4000, 1000, 8<<20 - 1, 10, false},
		{"no churn in the interval", 4000, 1000, 8 << 20, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, cur := writeSnaps(0, tc.upd, 0, 300, one, tc.live, tc.dead)
			cur.Tables[0].TableBytes = tc.bytes
			_, got := hasCase(detect(prev, cur), "write_amplification:public.events")
			if got != tc.want {
				t.Fatalf("case = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectCases_WriteAmplificationNeedsAnInterval(t *testing.T) {
	_, cur := writeSnaps(15000, 0, 0, 300, eventsIndexes(0), 100000, 2000)
	cs := DetectCases(cur, nil, ClassifyWorkload(cur, nil, t0), DefaultThresholds())
	if len(cs) != 0 {
		t.Fatalf("write rates need two snapshots: %v", caseIDs(cs))
	}
}

func TestDetectCases_TestSchemaTablesHaveNoWriteCase(t *testing.T) {
	prev, cur := writeSnaps(15000, 0, 0, 300, nil, 100000, 0)
	for _, s := range []*collector.Snapshot{prev, cur} {
		s.Tables[0].SchemaName = "test_abc"
	}
	idx := eventsIndexes(0)
	for i := range idx {
		idx[i].SchemaName = "test_abc"
	}
	prev.Indexes, cur.Indexes = idx, idx
	if cs := detect(prev, cur); len(cs) != 0 {
		t.Fatalf("test fixtures are not workload: %v", caseIDs(cs))
	}
}

func TestDetectCases_OrderAndCap(t *testing.T) {
	var prevQ, curQ []collector.QueryStats
	for i := int64(1); i <= 30; i++ {
		q := fmt.Sprintf("SELECT * FROM public.orders WHERE c%d = $1", i)
		prevQ = append(prevQ, stmt(i, q, 100, 1000))
		curQ = append(curQ, stmt(i, q, 200, 1000+float64(1000*i)))
	}
	// Statement i's interval mean is 10*i ms against a prior mean of 10 ms,
	// so statements 2..30 regress and statement 1 is a top statement.
	tbl := []collector.TableStats{table("public", "orders", 1000, 0)}
	prev := snapAt(t0, prevQ, tbl, nil)
	cur := snapAt(t0.Add(time.Minute), curQ, tbl, nil)
	th := DefaultThresholds()
	th.MinShare = 0.001
	cs := DetectCases(cur, prev, ClassifyWorkload(cur, nil, t0), th)
	if len(cs) != th.MaxCases {
		t.Fatalf("cases = %d, want the cap %d", len(cs), th.MaxCases)
	}
	if cs[0].Kind != CaseRegression {
		t.Fatalf("regressions come first: %v", caseIDs(cs))
	}
	for i := 2; i < len(cs); i++ {
		if cs[i].Kind == cs[i-1].Kind && cs[i].Weight > cs[i-1].Weight {
			t.Fatalf("within a kind, cases are by weight: %v", caseIDs(cs))
		}
	}
	again := DetectCases(cur, prev, ClassifyWorkload(cur, nil, t0), th)
	if fmt.Sprint(caseIDs(again)) != fmt.Sprint(caseIDs(cs)) {
		t.Fatal("case IDs and order are deterministic")
	}
}

func TestDetectCases_NilAndEmpty(t *testing.T) {
	if cs := DetectCases(nil, nil, Workload{}, DefaultThresholds()); len(cs) != 0 {
		t.Fatalf("nil snapshot: %v", cs)
	}
	empty := &collector.Snapshot{CollectedAt: t0}
	if cs := DetectCases(empty, empty, ClassifyWorkload(empty, nil, t0),
		DefaultThresholds()); len(cs) != 0 {
		t.Fatalf("empty snapshot: %v", cs)
	}
	// Zero-valued thresholds fall back to the defaults (no case for noise).
	prev, cur := shareSnaps(10, 1000, 5)
	if cs := DetectCases(cur, prev, ClassifyWorkload(cur, nil, t0), Thresholds{}); len(cs) != 0 {
		t.Fatalf("zero thresholds must mean defaults, got %v", caseIDs(cs))
	}
}
