package forecaster

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 4 (v1.8.3): the forecaster rebuilt the first
// and last snapshot of every day of its lookback in PL/pgSQL on every run
// (164-191 ms a call at the gate's scale, 30 days by default) and ranked
// every snapshot of the lookback to find them. The daily samples are now
// found by index, one probe per day, and a Forecaster decodes each sample
// once: a run decodes only the samples it has not seen (today's newest).

// recordingForecaster is a Forecaster over its own pool on base's
// database (base comes from phase2RequireDB, which a test calls once: it
// holds the cross-package lock) whose statements are recorded.
func recordingForecaster(t *testing.T, base *pgxpool.Pool) (*Forecaster,
	*testdb.QueryRecorder) {
	t.Helper()
	cfg := base.Config().Copy()
	rec := &testdb.QueryRecorder{}
	cfg.ConnConfig.Tracer = rec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool, ForecasterConfig{Enabled: true, LookbackDays: boundedDays + 1},
		func(string, string, ...any) {}), rec
}

// decoded counts the snapshots rebuilt through sage.snapshot_data by the
// recorded statements (each passes the ids it decodes first).
func decoded(t *testing.T, rec *testdb.QueryRecorder) int {
	t.Helper()
	n := 0
	for _, q := range rec.Matching("sage.snapshot_data(") {
		ids, ok := q.Args[0].([]int64)
		if !ok {
			t.Fatalf("decode statement's first argument is %T, want []int64", q.Args[0])
		}
		n += len(ids)
	}
	rec.Reset()
	return n
}

func TestForecaster_DecodesEachDailySampleOnce(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedSequenceSnapshots(t, ctx, pool)
	seedBoundedQuerySnapshots(t, ctx, pool)
	f, rec := recordingForecaster(t, pool)
	q1, err := f.dailyQueryAggs(ctx)
	if err != nil {
		t.Fatalf("query aggs: %v", err)
	}
	s1, err := f.dailySeqAggs(ctx)
	if err != nil {
		t.Fatalf("seq aggs: %v", err)
	}
	// Per day: the first and last query sample and the last sequence one.
	if n := decoded(t, rec); n < boundedDays || n > 3*boundedDays {
		t.Fatalf("cold run decoded %d snapshots, want at most %d", n, 3*boundedDays)
	}
	wantQ, err := QueryDailyQueryAggs(ctx, pool, boundedDays+1)
	if err != nil || !reflect.DeepEqual(q1, wantQ) {
		t.Fatalf("cached query aggs = %+v, fresh = %+v (%v)", q1, wantQ, err)
	}
	q2, err := f.dailyQueryAggs(ctx)
	if err != nil || !reflect.DeepEqual(q2, q1) {
		t.Fatalf("second run = %+v (%v), want %+v", q2, err, q1)
	}
	s2, err := f.dailySeqAggs(ctx)
	if err != nil || !reflect.DeepEqual(s2, s1) {
		t.Fatalf("second seq run = %+v (%v), want %+v", s2, err, s1)
	}
	if n := decoded(t, rec); n != 0 {
		t.Fatalf("a run with no new snapshot decoded %d, want 0", n)
	}
	// A newer query snapshot today (after every seeded one) replaces only
	// today's last sample.
	insertSnapshot(t, ctx, pool, dayStart(0).Add(23*time.Hour+30*time.Minute), "queries",
		[]map[string]any{{"queryid": 42, "calls": 100000}, {"queryid": 43, "calls": 7}})
	q3, err := f.dailyQueryAggs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := decoded(t, rec); n != 1 {
		t.Fatalf("a run with one new snapshot decoded %d, want 1", n)
	}
	wantQ, err = QueryDailyQueryAggs(ctx, pool, boundedDays+1)
	if err != nil || !reflect.DeepEqual(q3, wantQ) {
		t.Fatalf("cached query aggs after a new snapshot = %+v, fresh = %+v (%v)", q3,
			wantQ, err)
	}
}

// The daily samples are found by index, not by ranking every snapshot of
// the lookback: the picking statement plans no sequential scan of
// sage.snapshots (sequential scans disabled, it must not need one).
func TestForecaster_DailySamplesFoundByIndex(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedBoundedQuerySnapshots(t, ctx, pool)
	f, rec := recordingForecaster(t, pool)
	if _, err := f.dailyQueryAggs(ctx); err != nil {
		t.Fatal(err)
	}
	var picks []testdb.RecordedQuery
	for _, q := range rec.Matching("sage.snapshots") {
		if !strings.Contains(q.SQL, "sage.snapshot_data(") {
			picks = append(picks, q)
		}
	}
	if len(picks) == 0 {
		t.Fatal("no sample-picking statement recorded")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET enable_seqscan") }()
	for _, q := range picks {
		plan, err := testdb.Explain(ctx, conn, "", q.SQL, q.Args...)
		if err != nil {
			t.Fatal(err)
		}
		if plan.SeqScans("snapshots") != 0 || plan.Has(func(n testdb.PlanNode) bool {
			return n.NodeType == "WindowAgg"
		}) {
			t.Fatalf("sample picking ranks or scans snapshots:\n%s", plan)
		}
	}
}
