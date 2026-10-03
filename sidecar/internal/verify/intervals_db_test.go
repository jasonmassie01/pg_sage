package verify

import (
	"context"
	"testing"
	"time"
)

// QueryMeasurements reports the spread across sampling intervals, not only
// the window's mean, so the decision can tell a real change from noise.
func TestQueryMeasurementReportsIntervalSpread(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	const qid = int64(990_201)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_store WHERE queryid=$1",
			qid)
	}
	clean()
	t.Cleanup(clean)
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-2 * time.Hour)
	// Cumulative counters every 20 minutes: interval deltas of
	// (10 calls, 100 ms), (20, 400), (30, 300), (10, 100), (30, 300).
	calls := []int64{100, 110, 130, 160, 170, 200}
	totals := []float64{1000, 1100, 1500, 1800, 1900, 2200}
	for i := range calls {
		at := from.Add(time.Duration(i)*20*time.Minute + time.Minute)
		mustExec(t, ctx, pool, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, 0)`, at, qid, calls[i], totals[i])
	}
	got, err := NewPostgresObservationSource(pool).QueryMeasurements(ctx, []int64{qid},
		from, to)
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	want := Summarize([]Interval{{10, 100}, {20, 400}, {30, 300}, {10, 100}, {30, 300}})
	m := got[qid]
	if m.Samples != 100 || m.AverageLatency != want.AverageLatency {
		t.Fatalf("measurement = %+v, want 100 calls at %s", m, want.AverageLatency)
	}
	if m.Buckets != 5 {
		t.Fatalf("buckets = %d, want one per sampling interval (5)", m.Buckets)
	}
	diff := m.StdErr - want.StdErr
	if diff < -time.Microsecond || diff > time.Microsecond {
		t.Fatalf("stderr = %s, want %s", m.StdErr, want.StdErr)
	}
}

// A window spanning a statistics reset is still refused (R10): no calls
// and no buckets, so the comparison is insufficient evidence.
func TestQueryMeasurementRefusesWindowAcrossReset(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	const qid = int64(990_202)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_store WHERE queryid=$1",
			qid)
	}
	clean()
	t.Cleanup(clean)
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-time.Hour)
	for i, c := range []int64{500, 600, 20, 80} {
		at := from.Add(time.Duration(i)*10*time.Minute + time.Minute)
		mustExec(t, ctx, pool, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, 0)`, at, qid, c, float64(c)*2)
	}
	got, err := NewPostgresObservationSource(pool).QueryMeasurements(ctx, []int64{qid},
		from, to)
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	if m := got[qid]; m.Samples != 0 || m.Buckets != 0 {
		t.Fatalf("measurement across a reset = %+v, want no evidence", m)
	}
}

func TestBucketWidthScalesWithWindow(t *testing.T) {
	cases := []struct {
		window, want time.Duration
	}{
		{0, 5 * time.Second},
		{10 * time.Minute, 12500 * time.Millisecond},
		{2 * time.Minute, 5 * time.Second},
		{2 * time.Hour, 150 * time.Second},
		{7 * 24 * time.Hour, 3*time.Hour + 30*time.Minute},
		{60 * 24 * time.Hour, 6 * time.Hour},
	}
	for _, tc := range cases {
		if got := bucketWidth(tc.window); got != tc.want {
			t.Errorf("bucketWidth(%s) = %s, want %s", tc.window, got, tc.want)
		}
	}
}
