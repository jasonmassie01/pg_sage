package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01a review: the parts of an hour's rows the reconciliation fixture
// cannot see: a decreased secondary counter breaks its pair, a re-roll
// drops the rows of queryids gone from the hour, plan_hash is the newest
// fingerprint of the hour, dealloc_delta sums the growth, and
// SnapshotIntervals filters by queryid.

func TestRollupHour_DecreasedSecondaryCounterBreaksThePair(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	s := sys{epoch: &epoch}
	// An evicted entry that came back in the same epoch: calls and time grew
	// past the old values, but rows and WAL restarted lower.
	persistCycle(t, w, p.Monitored, hour.Add(5*time.Minute),
		[]qc{{id: 7, calls: 10, exec: 10, rows: 500, hit: 50, wal: 9000}}, s)
	persistCycle(t, w, p.Monitored, hour.Add(10*time.Minute),
		[]qc{{id: 7, calls: 12, exec: 11, rows: 3, hit: 60, wal: 100}}, s)
	persistCycle(t, w, p.Monitored, hour.Add(15*time.Minute),
		[]qc{{id: 7, calls: 15, exec: 14, rows: 9, hit: 70, wal: 160}}, s)
	rep, err := RollupHour(context.Background(), p.Monitored, hour)
	if err != nil {
		t.Fatalf("a decreased secondary counter must not fail the hour: %v", err)
	}
	r := readHourRows(t, p.Monitored, hour)[7]
	if !r.brk || r.pairs != 1 || r.calls != 3 || r.exec != 3 || r.rows != 6 ||
		r.hit != 10 || r.wal != 60 || rep.EpochBreaks != 1 {
		t.Fatalf("row = %+v, report %+v; want only the last pair, flagged", r, rep)
	}
}

func TestRollupHour_ReRollDropsQueryidsGoneFromTheHour(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	for i := range 3 {
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i+1)*10*time.Minute),
			[]qc{fixtureCounters(1, i), fixtureCounters(2, i)}, sys{epoch: &epoch})
	}
	if _, err := RollupHour(ctx, p.Monitored, hour); err != nil {
		t.Fatal(err)
	}
	if got := readHourRows(t, p.Monitored, hour); len(got) != 2 {
		t.Fatalf("first roll: %d rows, want 2", len(got))
	}
	if _, err := p.Monitored.Exec(ctx, `DELETE FROM sage.snapshots
		WHERE collected_at >= $1 AND collected_at < $2`, hour, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	w = snapstore.NewWriter()
	for i := range 2 {
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i+1)*10*time.Minute),
			[]qc{fixtureCounters(1, i)}, sys{epoch: &epoch})
	}
	rep, err := RollupHour(ctx, p.Monitored, hour)
	if err != nil {
		t.Fatal(err)
	}
	got := readHourRows(t, p.Monitored, hour)
	if _, stale := got[2]; stale || len(got) != 1 || rep.Queryids != 1 {
		t.Fatalf("re-roll kept %+v (report %+v); queryid 2 must be gone", got, rep)
	}
	if want := sub(fixtureCounters(1, 1), fixtureCounters(1, 0)); got[1].calls != want.calls {
		t.Fatalf("queryid 1 = %+v, want calls %d", got[1], want.calls)
	}
}

func TestRollupHour_PlanHashIsTheNewestOfTheHour(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	for i := range 2 {
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i+1)*10*time.Minute),
			[]qc{fixtureCounters(1, i), fixtureCounters(2, i)}, sys{epoch: &epoch})
	}
	for _, s := range []struct {
		at   time.Time
		hash string
	}{
		{hour.Add(-time.Minute), "before"}, {hour.Add(5 * time.Minute), "old"},
		{hour.Add(40 * time.Minute), "new"}, {hour.Add(time.Hour), "after"},
	} {
		if _, err := p.Monitored.Exec(ctx, `INSERT INTO sage.query_store (queryid,
			captured_at, calls, total_exec_time, mean_exec_time, rows, plan_hash)
			VALUES (1, $1, 1, 1, 1, 1, $2)`, s.at, s.hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RollupHour(ctx, p.Monitored, hour); err != nil {
		t.Fatal(err)
	}
	got := readHourRows(t, p.Monitored, hour)
	if h := got[1].planHash; h == nil || *h != "new" {
		t.Fatalf("queryid 1 plan_hash = %v, want the hour's newest (new)", h)
	}
	if h := got[2].planHash; h != nil {
		t.Fatalf("queryid 2 has no query store sample: plan_hash = %q, want NULL", *h)
	}
}

func TestRollupHour_DeallocDeltaSumsTheGrowthAndSkipsAReset(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	for i, d := range []int64{5, 7, 12, 2, 4} { // 2 + 5, then a reset, then 2
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i+1)*10*time.Minute),
			[]qc{fixtureCounters(1, i)},
			sys{epoch: &epoch, exec: float64(10 * (i + 1)), calls: int64(i + 1), dealloc: d})
	}
	if _, err := RollupHour(context.Background(), p.Monitored, hour); err != nil {
		t.Fatal(err)
	}
	c, ok := readCoverage(t, p.Monitored, hour)
	if !ok || c.dealloc == nil || *c.dealloc != 9 || c.dbExec == nil || *c.dbExec != 40 {
		t.Fatalf("coverage = %+v (dealloc %v, db %v); want dealloc 9 and db 40 ms", c,
			c.dealloc, c.dbExec)
	}
}

func TestSnapshotIntervals_FiltersByQueryid(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	times := make([]time.Time, 3)
	for i := range times {
		times[i] = hour.Add(time.Duration(i+1) * 10 * time.Minute)
		persistCycle(t, w, p.Monitored, times[i],
			[]qc{fixtureCounters(1, i), fixtureCounters(2, i), fixtureCounters(3, i)},
			sys{epoch: &epoch})
	}
	ivs, err := SnapshotIntervals(context.Background(), p.Monitored, hour,
		hour.Add(time.Hour), []int64{3, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(ivs) != 4 {
		t.Fatalf("%d intervals, want 2 for each of queryids 1 and 3: %+v", len(ivs), ivs)
	}
	for k, iv := range ivs {
		id, n := []int64{1, 1, 3, 3}[k], k%2
		want := sub(fixtureCounters(id, n+1), fixtureCounters(id, n))
		if iv.QueryID != id || !iv.From.Equal(times[n]) || !iv.To.Equal(times[n+1]) ||
			iv.EpochBreak || iv.Calls != want.calls || iv.WALBytes != want.wal {
			t.Fatalf("interval %d = %+v, want queryid %d from %v to %v, calls %d", k, iv,
				id, times[n], times[n+1], want.calls)
		}
	}
	if _, err := SnapshotIntervals(context.Background(), nil, hour, hour, nil); err == nil {
		t.Fatal("a nil store must fail")
	}
}
