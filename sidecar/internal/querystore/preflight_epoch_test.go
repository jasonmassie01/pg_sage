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
		if err := Record(ctx, p, []Sample{s}); err != nil {
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
		want    EvidenceStatus
		wantMs  float64
	}{
		{"same known epoch is measured", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, EvidenceMeasured, 20},
		{"epoch change after regrowth is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: b}}, EvidenceCountersReset, 0},
		{"unknown then known epoch is refused", []Sample{
			{Calls: 10, TotalExecMs: 100},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, EvidenceCountersReset, 0},
		{"known then unknown epoch is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 40, TotalExecMs: 700}}, EvidenceCountersReset, 0},
		{"interior epoch flip back is refused", []Sample{
			{Calls: 10, TotalExecMs: 100, StatsEpoch: a},
			{Calls: 20, TotalExecMs: 300, StatsEpoch: b},
			{Calls: 40, TotalExecMs: 700, StatsEpoch: a}}, EvidenceCountersReset, 0},
		{"legacy unknown epochs keep monotonic check", []Sample{
			{Calls: 10, TotalExecMs: 100},
			{Calls: 40, TotalExecMs: 700}}, EvidenceMeasured, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := preflightRecordSeries(t, ctx, p, 8902345, tc.samples)
			ev, err := WindowedLatencyEvidence(ctx, p, 8902345, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Status != tc.want || math.Abs(ev.LatencyMs-tc.wantMs) > 1e-9 {
				t.Fatalf("evidence = %+v, want %s %.1fms", ev, tc.want, tc.wantMs)
			}
			if ev.Samples != len(tc.samples) {
				t.Fatalf("samples = %d, want %d", ev.Samples, len(tc.samples))
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

func TestPreflightContractLegacyWindowUsesEpochs(t *testing.T) {
	p, ctx := requireDB(t)
	defer p.Close()
	from, _ := preflightRecordSeries(t, ctx, p, 8902347, []Sample{
		{Calls: 10, TotalExecMs: 100, StatsEpoch: preflightEpochA},
		{Calls: 40, TotalExecMs: 700, StatsEpoch: preflightEpochA}})
	ms, ok, err := WindowedLatencyMs(ctx, p, 8902347, from)
	if err != nil || !ok || math.Abs(ms-20) > 1e-9 {
		t.Fatalf("same epoch = (%v, %v, %v), want (20, true, nil)", ms, ok, err)
	}
	// The legacy window has no upper bound, so a later sample from a new
	// epoch joins it and the whole window is refused.
	if err := Record(ctx, p, []Sample{{QueryID: 8902347, Calls: 90,
		TotalExecMs: 1000, StatsEpoch: preflightEpochB}}); err != nil {
		t.Fatal(err)
	}
	ms, ok, err = WindowedLatencyMs(ctx, p, 8902347, from)
	if err != nil || ok || ms != 0 {
		t.Fatalf("cross epoch = (%v, %v, %v), want (0, false, nil)", ms, ok, err)
	}
	ms, ok, err = WindowedLatencyMs(ctx, p, 8902347, from.Add(time.Hour))
	if err != nil || ok || ms != 0 {
		t.Fatalf("no samples = (%v, %v, %v), want (0, false, nil)", ms, ok, err)
	}
}
