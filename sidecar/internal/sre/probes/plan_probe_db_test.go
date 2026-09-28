package probes

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// plan_regressions joins query_store.plan_hash flips with the windowed
// latency (delta total time / delta calls) before and after the flip.

type sample struct {
	ago   time.Duration
	calls int64
	total float64
	hash  string
}

func seedSamples(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64, ss []sample,
) {
	t.Helper()
	for _, s := range ss {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time, plan_hash)
			VALUES (now() - make_interval(secs => $1), $2, $3, $4, 0,
			NULLIF($5, ''))`,
			s.ago.Seconds(), qid, s.calls, s.total, s.hash); err != nil {
			t.Fatalf("seed sample: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.query_store WHERE queryid = $1", qid)
	})
}

func shiftFor(t *testing.T, shifts []PlanShift, qid int64) PlanShift {
	t.Helper()
	for _, s := range shifts {
		if s.QueryID == qid {
			return s
		}
	}
	t.Fatalf("no plan shift for queryid %d in %+v", qid, shifts)
	return PlanShift{}
}

func TestPlanRegressions_FlipJoinedWithWindowedLatency(t *testing.T) {
	pool, ctx := livePool(t)
	bootstrapQueryStore(t, ctx, pool)
	const flipQ, steadyQ, resetQ, nullQ = 881001, 881002, 881003, 881004
	seedSamples(t, ctx, pool, flipQ, []sample{
		{30 * time.Minute, 0, 0, "v1:a"}, {20 * time.Minute, 10, 5, "v1:a"},
		{10 * time.Minute, 20, 55, "v1:b"}})
	seedSamples(t, ctx, pool, steadyQ, []sample{
		{40 * time.Minute, 0, 0, "v1:c"}, {30 * time.Minute, 10, 10, "v1:c"},
		{20 * time.Minute, 20, 40, "v1:c"}, {10 * time.Minute, 30, 70, "v1:c"}})
	// A counter reset (calls fall) is never differenced.
	seedSamples(t, ctx, pool, resetQ, []sample{
		{30 * time.Minute, 100, 100, "v1:d"}, {20 * time.Minute, 5, 500, "v1:d"},
		{10 * time.Minute, 15, 510, "v1:d"}})
	// No fingerprint: plan shape unknown, so no row.
	seedSamples(t, ctx, pool, nullQ, []sample{
		{20 * time.Minute, 0, 0, ""}, {10 * time.Minute, 10, 100, ""}})

	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, PlanRegressions, Args{})
	shifts, err := PlanShifts(res)
	if err != nil {
		t.Fatalf("plan_regressions: %+v (%v)", res, err)
	}
	flip := shiftFor(t, shifts, flipQ)
	if !flip.Flipped || flip.PreviousHash != "v1:a" || flip.CurrentHash != "v1:b" ||
		flip.BeforeCalls != 10 || flip.AfterCalls != 10 ||
		flip.BeforeMeanMS != 0.5 || flip.AfterMeanMS != 5 || flip.Ratio() != 10 {
		t.Fatalf("flip shift = %+v", flip)
	}
	steady := shiftFor(t, shifts, steadyQ)
	if steady.Flipped || steady.CurrentHash != "v1:c" || steady.BeforeMeanMS != 1 ||
		steady.AfterMeanMS != 3 || steady.BeforeCalls != 10 || steady.AfterCalls != 20 {
		t.Fatalf("steady shift = %+v", steady)
	}
	for _, s := range shifts {
		if s.QueryID == nullQ {
			t.Fatalf("a query without plan fingerprints was reported: %+v", s)
		}
		if s.QueryID == resetQ && (s.BeforeCalls+s.AfterCalls != 10 ||
			math.Abs(s.AfterMeanMS-1) > 1e-9) {
			t.Fatalf("reset shift = %+v, want only the post-reset delta", s)
		}
	}
}

func TestPlanRegressions_WindowExcludesOldSamples(t *testing.T) {
	pool, ctx := livePool(t)
	bootstrapQueryStore(t, ctx, pool)
	const qid = 881010
	seedSamples(t, ctx, pool, qid, []sample{
		{3 * time.Hour, 0, 0, "v1:x"}, {2 * time.Hour, 10, 5, "v1:x"},
		{90 * time.Minute, 20, 500, "v1:y"}})
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	shifts, err := PlanShifts(r.Run(ctx, PlanRegressions, Args{Window: time.Hour}))
	if err != nil {
		t.Fatalf("plan_regressions: %v", err)
	}
	for _, s := range shifts {
		if s.QueryID == qid {
			t.Fatalf("samples older than the window were used: %+v", s)
		}
	}
	shifts, err = PlanShifts(r.Run(ctx, PlanRegressions, Args{Window: 4 * time.Hour}))
	if err != nil || !shiftFor(t, shifts, qid).Flipped {
		t.Fatalf("4h window must see the flip: %+v (%v)", shifts, err)
	}
}
