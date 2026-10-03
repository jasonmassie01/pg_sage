package verify

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// query_store is written only when a query's counters move (perf M11), so
// a verification window can open with no sample of an idle query: the last
// sample before the window (within querystore.AnchorLookback) is its start.
func TestQueryMeasurementAnchorsOnLastSampleBeforeWindow(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	const qid = int64(990_101)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_store WHERE queryid=$1",
			qid)
	}
	clean()
	t.Cleanup(clean)
	now := time.Now().UTC()
	from, to := now.Add(-30*time.Minute), now
	mustExec(t, ctx, pool, `INSERT INTO sage.query_store
		(captured_at, queryid, calls, total_exec_time, mean_exec_time) VALUES
		($1, $2, 100, 1000, 10), ($3, $2, 140, 2600, 18.6)`,
		from.Add(-20*time.Minute), qid, from.Add(10*time.Minute))
	got, err := NewPostgresObservationSource(pool).QueryMeasurements(ctx, []int64{qid}, from, to)
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	// 40 new calls, 1600 ms: 40 ms each.
	if m := got[qid]; m.Samples != 40 || m.AverageLatency != 40*time.Millisecond {
		t.Fatalf("measurement = %+v, want 40 calls at 40 ms", m)
	}
}

// A sample older than the lookback is not a window start.
func TestQueryMeasurementIgnoresAnchorBeyondLookback(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	const qid = int64(990_102)
	clean := func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.query_store WHERE queryid=$1",
			qid)
	}
	clean()
	t.Cleanup(clean)
	now := time.Now().UTC()
	from, to := now.Add(-30*time.Minute), now
	mustExec(t, ctx, pool, `INSERT INTO sage.query_store
		(captured_at, queryid, calls, total_exec_time, mean_exec_time) VALUES
		($1, $2, 100, 1000, 10), ($3, $2, 140, 2600, 18.6)`,
		from.Add(-querystore.AnchorLookback-time.Minute), qid, from.Add(10*time.Minute))
	got, err := NewPostgresObservationSource(pool).QueryMeasurements(ctx, []int64{qid}, from, to)
	if err != nil {
		t.Fatalf("QueryMeasurements: %v", err)
	}
	if m := got[qid]; m.Samples != 0 {
		t.Fatalf("measurement = %+v, want no evidence from a single in-window sample", m)
	}
}
