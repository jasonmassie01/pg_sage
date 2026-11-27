package querystore

import (
	"errors"
	"testing"
	"time"
)

// No concurrent access tests here: validation, ranking and history
// building are pure functions over their arguments; the readers' parallel
// use is covered against PostgreSQL (top_db_test.go).

var topFrom = time.Date(2020, 3, 1, 10, 0, 0, 0, time.UTC)

func TestValidateTop_RejectsBadRequests(t *testing.T) {
	ok := TopRequest{From: topFrom, To: topFrom.Add(time.Hour), Order: OrderLoad, Limit: 10}
	if err := ValidateTop(ok); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	cases := map[string]TopRequest{
		"limit zero":        {From: ok.From, To: ok.To, Order: OrderLoad, Limit: 0},
		"limit negative":    {From: ok.From, To: ok.To, Order: OrderLoad, Limit: -1},
		"limit past scan":   {From: ok.From, To: ok.To, Order: OrderLoad, Limit: MaxTopScan + 1},
		"unknown order":     {From: ok.From, To: ok.To, Order: "total_time", Limit: 5},
		"empty order":       {From: ok.From, To: ok.To, Order: "", Limit: 5},
		"until before from": {From: ok.To, To: ok.From, Order: OrderLoad, Limit: 5},
		"empty window":      {From: ok.From, To: ok.From, Order: OrderLoad, Limit: 5},
		"zero from":         {To: ok.To, Order: OrderLoad, Limit: 5},
		"zero to":           {From: ok.From, Order: OrderLoad, Limit: 5},
	}
	for name, req := range cases {
		err := ValidateTop(req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: ValidateTop = %v, want ErrInvalidRequest", name, err)
		}
	}
	edge := ok
	edge.Limit = MaxTopScan
	if err := ValidateTop(edge); err != nil {
		t.Errorf("limit at the scan bound refused: %v", err)
	}
}

func TestWindowStat_LoadAndMean(t *testing.T) {
	w := WindowStat{Calls: 300, TotalMs: 30_000, Intervals: 2}
	if got := w.Load(time.Minute); got != 0.5 {
		t.Errorf("load = %v, want 0.5 exec seconds per second", got)
	}
	if got := w.MeanMs(); got != 100 {
		t.Errorf("mean = %v, want 100 ms", got)
	}
	zero := WindowStat{}
	if zero.MeanMs() != 0 || zero.Load(0) != 0 || zero.Load(time.Minute) != 0 {
		t.Errorf("zero stat: mean %v load %v, want 0 without a division by zero",
			zero.MeanMs(), zero.Load(time.Minute))
	}
}

func rankFixture() []WindowStat {
	return []WindowStat{
		{QueryID: 1, Samples: 3, Intervals: 3, Calls: 10, TotalMs: 9000, Rows: 5},
		{QueryID: 2, Samples: 3, Intervals: 3, Calls: 9000, TotalMs: 4500, Rows: 90000},
		{QueryID: 3, Samples: 2, Intervals: 2, Calls: 2, TotalMs: 6000, Rows: 1},
		{QueryID: 4, Samples: 1, Intervals: 0}, // one sample: not measurable
		{QueryID: 5, Samples: 2, Intervals: 1, EpochBreaks: 1, Calls: 1, TotalMs: 1},
	}
}

func ids(stats []WindowStat) []int64 {
	out := make([]int64, len(stats))
	for i, s := range stats {
		out[i] = s.QueryID
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRankTop_OrdersAndCounts(t *testing.T) {
	want := map[string][]int64{
		OrderLoad:  {1, 3, 2, 5},
		OrderCalls: {2, 1, 3, 5},
		OrderMean:  {3, 1, 5, 2},
		OrderRows:  {2, 1, 3, 5},
	}
	for order, wantIDs := range want {
		req := TopRequest{From: topFrom, To: topFrom.Add(time.Hour), Order: order, Limit: 10}
		res := rankTop(rankFixture(), req)
		if got := ids(res.Queries); !sameIDs(got, wantIDs) {
			t.Errorf("order %s: ranking %v, want %v", order, got, wantIDs)
		}
		if res.NotMeasurable != 1 || res.EpochBreaks != 1 {
			t.Errorf("order %s: not measurable %d, breaks %d; want 1 and 1", order,
				res.NotMeasurable, res.EpochBreaks)
		}
		if res.Coverage != CoveragePartial {
			t.Errorf("order %s: coverage %q, want partial", order, res.Coverage)
		}
		if res.Window != time.Hour {
			t.Errorf("order %s: window %v, want 1h", order, res.Window)
		}
	}
}

func TestRankTop_LimitAndTies(t *testing.T) {
	stats := []WindowStat{
		{QueryID: 9, Samples: 2, Intervals: 1, TotalMs: 100},
		{QueryID: -4, Samples: 2, Intervals: 1, TotalMs: 100},
		{QueryID: 7, Samples: 2, Intervals: 1, TotalMs: 50},
	}
	req := TopRequest{From: topFrom, To: topFrom.Add(time.Hour), Order: OrderLoad, Limit: 2}
	res := rankTop(stats, req)
	if got := ids(res.Queries); !sameIDs(got, []int64{-4, 9}) {
		t.Fatalf("ranking %v, want the tie broken by queryid and cut at 2: [-4 9]", got)
	}
	if res.Coverage != CoverageFull {
		t.Errorf("coverage %q, want full", res.Coverage)
	}
}

func TestRankTop_CoverageStates(t *testing.T) {
	req := TopRequest{From: topFrom, To: topFrom.Add(time.Hour), Order: OrderLoad, Limit: 5}
	none := rankTop(nil, req)
	if none.Coverage != CoverageNone || none.Queries == nil || len(none.Queries) != 0 {
		t.Fatalf("no samples: coverage %q queries %#v, want none and an empty list",
			none.Coverage, none.Queries)
	}
	single := rankTop([]WindowStat{{QueryID: 1, Samples: 1}}, req)
	if single.Coverage != CoverageNotMeasurable || len(single.Queries) != 0 ||
		single.NotMeasurable != 1 {
		t.Fatalf("one sample: %+v, want not_measurable with no ranked query", single)
	}
	onlyBreak := rankTop([]WindowStat{{QueryID: 1, Samples: 2, EpochBreaks: 1}}, req)
	if onlyBreak.Coverage != CoverageNotMeasurable || len(onlyBreak.Queries) != 0 {
		t.Fatalf("only a break: %+v, want not_measurable", onlyBreak)
	}
}

func strp(s string) *string { return &s }

func at(m int) time.Time { return topFrom.Add(time.Duration(m) * time.Minute) }

func TestBuildHistory_BucketsAndBreaks(t *testing.T) {
	points := []samplePoint{
		{at: at(-30), inWindow: false},
		{at: at(1), inWindow: true, hasPrev: true, dCalls: 10, dTotal: 100, dRows: 4},
		{at: at(4), inWindow: true, hasPrev: true, dCalls: 30, dTotal: 500, dRows: 6},
		{at: at(6), inWindow: true, hasPrev: true, brk: true, dCalls: -40, dTotal: -600},
		{at: at(8), inWindow: true, hasPrev: true, dCalls: 5, dTotal: 50, dRows: 1},
	}
	h := buildHistory(points, 5*time.Minute)
	if len(h.Buckets) != 2 {
		t.Fatalf("buckets %+v, want 2 (minutes 0-5 and 5-10)", h.Buckets)
	}
	b0, b1 := h.Buckets[0], h.Buckets[1]
	if !b0.Start.Equal(topFrom) || b0.Calls != 40 || b0.TotalMs != 600 || b0.Rows != 10 ||
		b0.MeanMs != 15 || b0.EpochBreak {
		t.Errorf("bucket 0 = %+v, want 40 calls 600 ms 10 rows mean 15", b0)
	}
	if !b1.Start.Equal(at(5)) || b1.Calls != 5 || b1.TotalMs != 50 || !b1.EpochBreak {
		t.Errorf("bucket 1 = %+v, want 5 calls 50 ms with the break flagged and "+
			"its negative delta left out", b1)
	}
	if h.Coverage != CoveragePartial || h.Samples != 4 {
		t.Errorf("coverage %q samples %d, want partial and 4 in-window samples",
			h.Coverage, h.Samples)
	}
}

func TestBuildHistory_EmptyAndSingle(t *testing.T) {
	h := buildHistory(nil, time.Minute)
	if h.Coverage != CoverageNone || h.Buckets == nil || h.Markers == nil ||
		len(h.Buckets)+len(h.Markers) != 0 {
		t.Fatalf("no samples: %+v, want none with empty lists", h)
	}
	one := buildHistory([]samplePoint{{at: at(1), inWindow: true}}, time.Minute)
	if one.Coverage != CoverageNotMeasurable || len(one.Buckets) != 0 || one.Samples != 1 {
		t.Fatalf("one sample: %+v, want not_measurable", one)
	}
}

func TestPlanMarkers_OneMarkerPerChange(t *testing.T) {
	points := []samplePoint{
		{at: at(-10), planHash: strp("A")},               // before the window
		{at: at(1), inWindow: true, planHash: strp("A")}, // unchanged
		{at: at(2), inWindow: true},                      // not fingerprinted
		{at: at(3), inWindow: true, planHash: strp("B")}, // A -> B
		{at: at(4), inWindow: true, planHash: strp("B")},
	}
	got := planMarkers(points)
	if len(got) != 1 || got[0].From != "A" || got[0].To != "B" || !got[0].At.Equal(at(3)) {
		t.Fatalf("markers %+v, want one A->B at minute 3", got)
	}
	first := planMarkers([]samplePoint{{at: at(1), inWindow: true, planHash: strp("A")}})
	if len(first) != 0 {
		t.Errorf("first fingerprint gave markers %+v, want none", first)
	}
	before := planMarkers([]samplePoint{
		{at: at(-5), planHash: strp("A")}, {at: at(-1), planHash: strp("B")},
	})
	if len(before) != 0 {
		t.Errorf("a change before the window gave %+v, want none", before)
	}
}
