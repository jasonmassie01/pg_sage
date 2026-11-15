package querystore

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01a: the rollup worker's states on a real server: backfill in
// 24-hour passes, caught up, steady state (h-1 at :05 and h-2 re-rolled,
// so a late snapshot reaches its hour), and a stop that ends Run.

func rolledHours(t *testing.T, db any) int {
	t.Helper()
	var n int
	if err := histstore.Resolve(db).QueryRow(context.Background(),
		`SELECT count(*) FROM sage.query_rollup_coverage c WHERE {db:c}`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRollupWorker_BackfillCatchUpSteadyAndLateSnapshot(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	base, epoch := pastHour(80), pastHour(100)
	w := snapstore.NewWriter()
	for i := range 144 { // 3 days, every 30 minutes
		persistCycle(t, w, p.Monitored, base.Add(time.Duration(i)*30*time.Minute),
			[]qc{fixtureCounters(1, i), fixtureCounters(2, i)}, sys{epoch: &epoch})
	}
	now := base.Add(72*time.Hour + 10*time.Minute)
	worker := &RollupWorker{Store: p.Monitored, Database: "rollup_fixture",
		Clock: func() time.Time { return now }}
	for pass, want := range []int{24, 48, 72, 72} {
		n, err := worker.Pass(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if got := rolledHours(t, p.Monitored); got != want || (pass < 3 && n != 24) {
			t.Fatalf("pass %d rolled %d (total %d), want total %d", pass, n, got, want)
		}
	}
	if lag := rollupLagOf("rollup_fixture"); lag != 0 {
		t.Fatalf("caught up: lag %v hours, want 0", lag)
	}
	checkSteadyAndLate(t, worker, w, p, base, &now)
}

// checkSteadyAndLate writes a late snapshot into the newest rolled hour,
// then moves the clock past the next hour: nothing is due before :05, and
// at :05 the pass rolls h-1 and re-rolls h-2, which takes the late cycle.
func checkSteadyAndLate(t *testing.T, worker *RollupWorker, w *snapstore.Writer,
	p *histfixture.Pair, base time.Time, now *time.Time) {
	t.Helper()
	ctx, epoch := context.Background(), pastHour(100)
	late := base.Add(71 * time.Hour)
	before := readHourRows(t, p.Monitored, late)[1]
	persistCycle(t, w, p.Monitored, late.Add(50*time.Minute),
		[]qc{{id: 1, calls: before.calls * 100, exec: 1e6, rows: 1e6, hit: 1e6, read: 1e6,
			tread: 1e6, twrite: 1e6, wal: 1e9}}, sys{epoch: &epoch})
	*now = base.Add(73*time.Hour + 4*time.Minute)
	if n, err := worker.Pass(ctx); err != nil || n != 0 {
		t.Fatalf("before :05 the new hour is not due: rolled %d, %v", n, err)
	}
	*now = base.Add(73*time.Hour + 5*time.Minute)
	n, err := worker.Pass(ctx)
	if err != nil || n != 2 {
		t.Fatalf("steady pass at :05 rolled %d (%v), want h-1 and h-2", n, err)
	}
	after := readHourRows(t, p.Monitored, late)[1]
	if after.calls <= before.calls || after.pairs != before.pairs+1 {
		t.Fatalf("the late snapshot must change h-2 on the re-roll: %+v -> %+v", before,
			after)
	}
	if c, ok := readCoverage(t, p.Monitored, base.Add(72*time.Hour)); !ok || c.snapshots != 0 {
		t.Fatalf("h-1 without snapshots: coverage %+v, %v", c, ok)
	}
	if rollupCountOf("rollup_fixture", "ok") < 72 || rollupCountOf("rollup_fixture",
		"empty") < 1 {
		t.Fatalf("hour counters: %+v", RollupHourCounts())
	}
}

func TestRollupWorker_NoSnapshotsRollsNothing(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	worker := &RollupWorker{Store: p.Monitored, Database: "rollup_none"}
	n, err := worker.Pass(context.Background())
	if err != nil || n != 0 || rolledHours(t, p.Monitored) != 0 {
		t.Fatalf("no snapshots: rolled %d, %v", n, err)
	}
}

func TestRollupWorker_StopEndsRunWithinOneTick(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	ctx, cancel := context.WithCancel(context.Background())
	worker := &RollupWorker{Store: p.Monitored, Database: "rollup_stop",
		Tick: 50 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the runtime stopped")
	}
}

func rollupLagOf(database string) float64 {
	for _, l := range RollupLags() {
		if l.Database == database {
			return l.Hours
		}
	}
	return -1
}

func rollupCountOf(database, result string) uint64 {
	for _, c := range RollupHourCounts() {
		if c.Database == database && c.Result == result {
			return c.Count
		}
	}
	return 0
}
