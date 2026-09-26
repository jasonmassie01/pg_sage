package verify

import (
	"testing"
	"time"
)

// No concurrent access tests: queryMeasurement is a single read-only
// statement; ordering of the stored samples is the subject here.

func TestPreflightContractVerifyRefusesCrossEpochWindow(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	const qid = int64(990033)
	from := time.Date(2099, 7, 23, 12, 0, 0, 0, time.UTC)
	epochA := time.Date(2099, 7, 1, 0, 0, 0, 0, time.UTC)
	epochB := epochA.Add(24 * time.Hour)
	type row struct {
		calls int64
		total float64
		epoch *time.Time
	}
	cases := []struct {
		name        string
		rows        []row
		wantSamples int
		wantLatency time.Duration
	}{
		{"same epoch is measured", []row{{10, 100, &epochA}, {40, 400, &epochA}},
			30, 10 * time.Millisecond},
		{"epoch change after regrowth yields no samples",
			[]row{{10, 100, &epochA}, {40, 400, &epochB}}, 0, 0},
		{"unknown next to known epoch yields no samples",
			[]row{{10, 100, nil}, {40, 400, &epochA}}, 0, 0},
		{"interior counter reset yields no samples",
			[]row{{10, 100, &epochA}, {1, 5, &epochA}, {40, 400, &epochA}}, 0, 0},
		{"legacy rows without epoch keep endpoint arithmetic",
			[]row{{10, 100, nil}, {40, 400, nil}}, 30, 10 * time.Millisecond},
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", qid)
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustExec(t, ctx, pool, "DELETE FROM sage.query_store WHERE queryid=$1", qid)
			for i, r := range tc.rows {
				mustExec(t, ctx, pool, `INSERT INTO sage.query_store
					(captured_at, queryid, calls, total_exec_time, mean_exec_time,
					 stats_epoch) VALUES ($1, $2, $3, $4, 0, $5)`,
					from.Add(time.Duration(i)*time.Second), qid, r.calls, r.total, r.epoch)
			}
			source := NewPostgresObservationSource(pool)
			got, err := source.QueryMeasurements(ctx, []int64{qid}, from,
				from.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			m := got[qid]
			if m.Samples != tc.wantSamples || m.AverageLatency != tc.wantLatency {
				t.Fatalf("measurement = %+v, want samples %d latency %v",
					m, tc.wantSamples, tc.wantLatency)
			}
		})
	}
}
