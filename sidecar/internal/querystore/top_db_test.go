package querystore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// The workload explorer's readers (market-01) against PostgreSQL. Each
// test reads its own fixed window in the past, so no other test's samples
// (all written near now()) are ranked with its own.

type topSeed struct {
	qid   int64
	at    time.Time
	calls int64
	total float64
	rows  int64
	epoch *time.Time
	plan  *string
}

func seedTop(t *testing.T, ctx context.Context, pool *pgxpool.Pool, seeds []topSeed) {
	t.Helper()
	for _, s := range seeds {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
			(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows,
			 stats_epoch, plan_hash) VALUES ($1, $2, $3, $4, 0, $5, $6, $7)`,
			s.qid, s.at, s.calls, s.total, s.rows, s.epoch, s.plan); err != nil {
			t.Fatalf("seed sample %+v: %v", s, err)
		}
	}
}

func topWindow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, from time.Time,
	lo, hi int64) TopRequest {
	t.Helper()
	clearQueryIDs(t, ctx, pool, lo, hi)
	return TopRequest{From: from, To: from.Add(time.Hour), Order: OrderLoad, Limit: 10}
}

// A query idle for the window's start has no sample there: its anchor is
// the last sample before the window, and a query written only by
// keyframes (unchanged counters) is measurable with no load.
func TestTop_KeyframeAndAbsentSampleDeltasRankByLoad(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2020, 4, 1, 10, 0, 0, 0, time.UTC)
	req := topWindow(t, ctx, pool, from, 7_400_000_001, 7_400_000_003)
	m := func(n int) time.Time { return from.Add(time.Duration(n) * time.Minute) }
	seedTop(t, ctx, pool, []topSeed{
		// A: anchored 30 min before the window, one sample inside: 60 s.
		{qid: 7_400_000_001, at: m(-30), calls: 100, total: 1000, rows: 10},
		{qid: 7_400_000_001, at: m(50), calls: 160, total: 61_000, rows: 70},
		// B: sampled every 20 min, 30 s in all.
		{qid: 7_400_000_002, at: m(0), calls: 0, total: 0},
		{qid: 7_400_000_002, at: m(20), calls: 500, total: 10_000, rows: 500},
		{qid: 7_400_000_002, at: m(40), calls: 1000, total: 20_000, rows: 1000},
		{qid: 7_400_000_002, at: m(60), calls: 1500, total: 30_000, rows: 1500},
		// C: hourly keyframes with unchanged counters.
		{qid: 7_400_000_003, at: m(-50), calls: 7, total: 70},
		{qid: 7_400_000_003, at: m(10), calls: 7, total: 70},
	})
	res, err := Top(ctx, pool, req)
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if got := ids(res.Queries); !sameIDs(got, []int64{7_400_000_001, 7_400_000_002,
		7_400_000_003}) {
		t.Fatalf("load ranking %v, want A, B, C", got)
	}
	a, b, c := res.Queries[0], res.Queries[1], res.Queries[2]
	if a.Calls != 60 || a.TotalMs != 60_000 || a.Rows != 60 || a.Intervals != 1 {
		t.Errorf("A = %+v, want 60 calls 60000 ms 60 rows over 1 interval", a)
	}
	if got := a.Load(res.Window); got < 0.0166 || got > 0.0167 {
		t.Errorf("A load = %v, want 60 s / 3600 s", got)
	}
	if b.Calls != 1500 || b.TotalMs != 30_000 || b.Intervals != 3 {
		t.Errorf("B = %+v, want 1500 calls 30000 ms over 3 intervals", b)
	}
	if c.Calls != 0 || c.TotalMs != 0 || c.Intervals != 1 {
		t.Errorf("C = %+v, want measured with nothing done", c)
	}
	if res.Coverage != CoverageFull {
		t.Errorf("coverage %q, want full", res.Coverage)
	}
	req.Order = OrderCalls
	byCalls, err := Top(ctx, pool, req)
	if err != nil || byCalls.Queries[0].QueryID != 7_400_000_002 {
		t.Fatalf("calls ranking %v (%v), want B first", ids(byCalls.Queries), err)
	}
}

// A pg_stat_statements reset mid-window splits the window: the deltas on
// each side count, the pair across the reset does not.
func TestTop_EpochResetSplitsTheWindow(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2020, 4, 2, 10, 0, 0, 0, time.UTC)
	req := topWindow(t, ctx, pool, from, 7_400_000_010, 7_400_000_010)
	e1 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	e2 := from.Add(15 * time.Minute)
	m := func(n int) time.Time { return from.Add(time.Duration(n) * time.Minute) }
	seedTop(t, ctx, pool, []topSeed{
		{qid: 7_400_000_010, at: m(0), calls: 100, total: 1000, epoch: &e1},
		{qid: 7_400_000_010, at: m(10), calls: 200, total: 2000, epoch: &e1},
		{qid: 7_400_000_010, at: m(20), calls: 50, total: 500, epoch: &e2},
		{qid: 7_400_000_010, at: m(30), calls: 150, total: 1500, epoch: &e2},
	})
	res, err := Top(ctx, pool, req)
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if len(res.Queries) != 1 {
		t.Fatalf("queries %+v, want the one split query", res.Queries)
	}
	q := res.Queries[0]
	if q.Calls != 200 || q.TotalMs != 2000 || q.Intervals != 2 || q.EpochBreaks != 1 {
		t.Errorf("split query = %+v, want 200 calls 2000 ms from 2 intervals, 1 break", q)
	}
	if res.Coverage != CoveragePartial || res.EpochBreaks != 1 {
		t.Errorf("coverage %q breaks %d, want partial with 1 break", res.Coverage,
			res.EpochBreaks)
	}
}

// One sample and no anchor: the query's window is not measurable, and
// the response says so rather than ranking a zero.
func TestTop_FewerThanTwoSamplesIsNotMeasurable(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2020, 4, 3, 10, 0, 0, 0, time.UTC)
	req := topWindow(t, ctx, pool, from, 7_400_000_020, 7_400_000_020)
	// An anchor older than AnchorLookback does not start the window.
	seedTop(t, ctx, pool, []topSeed{
		{qid: 7_400_000_020, at: from.Add(-3 * time.Hour), calls: 1, total: 1},
		{qid: 7_400_000_020, at: from.Add(5 * time.Minute), calls: 9, total: 900},
	})
	res, err := Top(ctx, pool, req)
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if len(res.Queries) != 0 || res.NotMeasurable != 1 ||
		res.Coverage != CoverageNotMeasurable {
		t.Fatalf("result %+v, want no ranked query, 1 not measurable", res)
	}
}

func TestTop_NoSamplesIsCoverageNone(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2019, 6, 1, 10, 0, 0, 0, time.UTC)
	res, err := Top(ctx, pool, TopRequest{From: from, To: from.Add(time.Hour),
		Order: OrderLoad, Limit: 10})
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if res.Coverage != CoverageNone || res.Queries == nil || len(res.Queries) != 0 {
		t.Fatalf("result %+v, want coverage none and an empty list", res)
	}
}

func TestTop_InvalidRequestIsRefusedBeforeReading(t *testing.T) {
	_, err := Top(context.Background(), nil, TopRequest{Order: OrderLoad, Limit: 1})
	if err == nil {
		t.Fatal("Top with a zero window: nil error, want ErrInvalidRequest")
	}
}

func TestQueryHistory_PlanHashChangeIsOneMarker(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2020, 4, 4, 10, 0, 0, 0, time.UTC)
	clearQueryIDs(t, ctx, pool, 7_400_000_030, 7_400_000_030)
	m := func(n int) time.Time { return from.Add(time.Duration(n) * time.Minute) }
	a, b := "aaaa", "bbbb"
	seedTop(t, ctx, pool, []topSeed{
		{qid: 7_400_000_030, at: m(-10), calls: 0, total: 0, plan: &a},
		{qid: 7_400_000_030, at: m(10), calls: 10, total: 100, rows: 5, plan: &a},
		{qid: 7_400_000_030, at: m(20), calls: 20, total: 300, rows: 10, plan: &b},
		{qid: 7_400_000_030, at: m(40), calls: 30, total: 600, rows: 15, plan: &b},
	})
	h, err := QueryHistory(ctx, pool, 7_400_000_030, from, from.Add(time.Hour),
		30*time.Minute)
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(h.Markers) != 1 || h.Markers[0].From != a || h.Markers[0].To != b ||
		!h.Markers[0].At.Equal(m(20)) {
		t.Fatalf("markers %+v, want one aaaa->bbbb at minute 20", h.Markers)
	}
	if len(h.Buckets) != 2 || h.Buckets[0].Calls != 20 || h.Buckets[0].TotalMs != 300 ||
		h.Buckets[0].Rows != 10 || h.Buckets[1].Calls != 10 || h.Buckets[1].MeanMs != 30 {
		t.Fatalf("buckets %+v, want 20 calls/300 ms/10 rows then 10 calls mean 30 ms",
			h.Buckets)
	}
	if h.Coverage != CoverageFull || h.Samples != 3 {
		t.Errorf("coverage %q samples %d, want full and 3", h.Coverage, h.Samples)
	}
}

func TestQueryHistory_RejectsBadWindow(t *testing.T) {
	_, err := QueryHistory(context.Background(), nil, 1, topFrom, topFrom, time.Minute)
	if err == nil {
		t.Fatal("empty window: nil error, want ErrInvalidRequest")
	}
	_, err = QueryHistory(context.Background(), nil, 1, topFrom, topFrom.Add(time.Hour), 0)
	if err == nil {
		t.Fatal("zero bucket: nil error, want ErrInvalidRequest")
	}
}

// Parallel readers share nothing but the pool: every one gets the same
// ranking.
func TestTop_ParallelReadersAgree(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	from := time.Date(2020, 4, 5, 10, 0, 0, 0, time.UTC)
	req := topWindow(t, ctx, pool, from, 7_400_000_040, 7_400_000_041)
	seedTop(t, ctx, pool, []topSeed{
		{qid: 7_400_000_040, at: from, calls: 0, total: 0},
		{qid: 7_400_000_040, at: from.Add(time.Minute), calls: 1, total: 50},
		{qid: 7_400_000_041, at: from, calls: 0, total: 0},
		{qid: 7_400_000_041, at: from.Add(time.Minute), calls: 1, total: 90},
	})
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Top(ctx, pool, req)
			if err != nil || !sameIDs(ids(res.Queries), []int64{7_400_000_041,
				7_400_000_040}) {
				errs <- "parallel Top: " + errString(err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func errString(err error) string {
	if err == nil {
		return "wrong ranking"
	}
	return err.Error()
}

// Fleet isolation: in meta mode another database's samples of the same
// queryid (Noise) are neither ranked nor differenced with this one's.
func TestTop_AgreesInBothHistoryModes(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	const qid = int64(7_400_000_050)
	from := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	got := map[histstore.Mode]TopResult{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		target, id := p.Monitored, 0
		if mode == histstore.ModeMeta {
			target, id = p.Meta, histfixture.DatabaseID
			p.Noise(t, from.Add(5*time.Minute), qid, qid+1)
			p.Noise(t, from.Add(25*time.Minute), qid, qid+1)
		}
		insertScoped(t, ctx, target, qid, from.Add(time.Minute), 10, 100, id)
		insertScoped(t, ctx, target, qid, from.Add(20*time.Minute), 40, 700, id)
		res, err := Top(ctx, p.Monitored, TopRequest{From: from, To: from.Add(time.Hour),
			Order: OrderLoad, Limit: 10})
		if err != nil {
			t.Fatalf("%s Top: %v", mode, err)
		}
		got[mode] = res
	}
	for mode, res := range got {
		if len(res.Queries) != 1 || res.Queries[0].QueryID != qid ||
			res.Queries[0].Calls != 30 || res.Queries[0].TotalMs != 600 {
			t.Fatalf("%s top = %+v, want only this database's query: 30 calls 600 ms",
				mode, res.Queries)
		}
	}
	h, err := QueryHistory(ctx, p.Monitored, qid, from, from.Add(time.Hour), time.Hour)
	if err != nil || len(h.Buckets) != 1 || h.Buckets[0].Calls != 30 || len(h.Markers) != 0 {
		t.Fatalf("meta history %+v (%v), want 30 calls and no noise plan change", h, err)
	}
}
