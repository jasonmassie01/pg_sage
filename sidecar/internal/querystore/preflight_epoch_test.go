package querystore

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests: these fixtures test ordered sample
// semantics of one queryid, not writer contention.

var (
	preflightEpochA = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	preflightEpochB = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
)

func preflightRecordSeries(
	t *testing.T, ctx context.Context, p *pgxpool.Pool, id int64, samples []Sample,
) (time.Time, time.Time) {
	t.Helper()
	if _, err := p.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", id); err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		s.QueryID = id
		if err := recordNow(ctx, p, []Sample{s}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var now time.Time
	if err := p.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.Add(-time.Minute), now
}

func TestPreflightContractEpochWindows(t *testing.T) {
	p, ctx := requireDB(t)
	defer p.Close()
	a, b := preflightEpochA, preflightEpochB
	cases := []struct {
		name    string
		samples []Sample
		broken  bool
		wantMs  float64
	}{
		{"same known epoch is measured", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, false, 20},
		{"epoch change after regrowth is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: b}}, true, 0},
		{"unknown then known epoch is refused", []Sample{
			{Calls: 10, TotalExecMs: 100},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, true, 0},
		{"known then unknown epoch is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700}}, true, 0},
		{"interior epoch flip back is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 20, TotalExecMs: 300, StatsEpoch: b},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, true, 0},
		{"legacy unknown epochs keep monotonic check", []Sample{
			{Calls: 10, TotalExecMs: 100},
			{Calls: 40, TotalExecMs: 700}}, false, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := preflightRecordSeries(t, ctx, p, 8902345, tc.samples)
			w := windowOf(t, ctx, p, 8902345, from, to)
			ms, ok := w.measured()
			if w.broken != tc.broken || ok == tc.broken || math.Abs(ms-tc.wantMs) > 1e-9 {
				t.Fatalf("window = %+v (%.1f ms), want broken=%v %.1fms", w, ms, tc.broken,
					tc.wantMs)
			}
			if w.calls != 30 {
				t.Fatalf("calls = %d, want 30 across the window", w.calls)
			}
		})
	}
}

func TestPreflightContractRecordStoresEpoch(t *testing.T) {
	p, ctx := requireDB(t)
	defer p.Close()
	preflightRecordSeries(t, ctx, p, 8902346, []Sample{
		{Calls: 1, TotalExecMs: 1, StatsEpoch: preflightEpochA},
		{Calls: 2, TotalExecMs: 2}})
	rows, err := p.Query(ctx, `SELECT stats_epoch FROM sage.query_store
		WHERE queryid=$1 ORDER BY captured_at, id`, int64(8902346))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []*time.Time
	for rows.Next() {
		var e *time.Time
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] == nil || !got[0].Equal(preflightEpochA) || got[1] != nil {
		t.Fatalf("stored epochs = %v, want [%v <nil>]", got, preflightEpochA)
	}
}

func TestPreflightContractLaterEpochJoinsWindow(t *testing.T) {
	p, ctx := requireDB(t)
	defer p.Close()
	from, to := preflightRecordSeries(t, ctx, p, 8902347, []Sample{
		{Calls: 10, TotalExecMs: 100, StatsEpoch: preflightEpochA},
		{Calls: 40, TotalExecMs: 700, StatsEpoch: preflightEpochA}})
	if ms, ok := windowOf(t, ctx, p, 8902347, from, to).measured(); !ok ||
		math.Abs(ms-20) > 1e-9 {
		t.Fatalf("same epoch = (%v, %v), want (20, true)", ms, ok)
	}
	// A later sample from a new epoch inside the window refuses all of it.
	if err := recordNow(ctx, p, []Sample{{QueryID: 8902347, Calls: 90,
		TotalExecMs: 1000, StatsEpoch: preflightEpochB}}); err != nil {
		t.Fatal(err)
	}
	later := to.Add(time.Minute)
	if w := windowOf(t, ctx, p, 8902347, from, later); !w.broken {
		t.Fatalf("cross epoch window = %+v, want broken", w)
	}
	if w := windowOf(t, ctx, p, 8902347, later.Add(time.Hour),
		later.Add(2*time.Hour)); w != (window{}) {
		t.Fatalf("window after every sample = %+v, want empty", w)
	}
}
