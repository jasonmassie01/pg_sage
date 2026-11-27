package querystore

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01a on a real server, in both history placements: the hourly rollup
// of the snapshot queries category equals the snapshot counters, and the
// coverage row says how much of the database the rollup saw.

const fixtureQueryIDs = 20

// writeThreeHours writes 36 cycles, five minutes apart, of 20 queryids
// from the start of pastHour(5), with recorded epochs.
func writeThreeHours(t *testing.T, p *histfixture.Pair) time.Time {
	t.Helper()
	start, epoch := pastHour(5), pastHour(30)
	w := snapstore.NewWriter()
	for i := range 36 {
		qs := make([]qc, fixtureQueryIDs)
		for j := range qs {
			qs[j] = fixtureCounters(int64(j+1), i)
		}
		persistCycle(t, w, p.Monitored, start.Add(time.Duration(i)*5*time.Minute), qs,
			sys{epoch: &epoch, exec: 1000 * float64(i+1), calls: int64(50 * (i + 1)),
				dealloc: 5})
	}
	return start
}

// checkReconciled requires each hour's rows to be the last-minus-first
// counters of each queryid in it: exactly for integers, to 1e-9 relative
// for execution time (CHECK-02).
func checkReconciled(t *testing.T, got map[int64]hourRow, k int) {
	t.Helper()
	last, first := 12*k+11, max(12*k-1, 0)
	if len(got) != fixtureQueryIDs {
		t.Fatalf("hour %d: %d rows, want %d", k, len(got), fixtureQueryIDs)
	}
	for id := int64(1); id <= fixtureQueryIDs; id++ {
		want := sub(fixtureCounters(id, last), fixtureCounters(id, first))
		r := got[id]
		if r.calls != want.calls || r.rows != want.rows || r.hit != want.hit ||
			r.read != want.read || r.tread != want.tread || r.twrite != want.twrite ||
			r.wal != want.wal || r.brk || r.pairs != last-first {
			t.Fatalf("hour %d queryid %d = %+v, want %+v with %d pairs", k, id, r, want,
				last-first)
		}
		if math.Abs(r.exec-want.exec)/want.exec > 1e-9 {
			t.Fatalf("hour %d queryid %d exec %v, want %v", k, id, r.exec, want.exec)
		}
	}
}

func TestRollupHour_ReconcilesWithTheSnapshotsInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	got := map[histstore.Mode][]map[int64]hourRow{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateRollup(t, p)
		start := writeThreeHours(t, p)
		p.Noise(t, start.Add(10*time.Minute), 1, 2, 3)
		for k := range 3 {
			hour := start.Add(time.Duration(k) * time.Hour)
			rep, err := RollupHour(ctx, p.Monitored, hour)
			if err != nil {
				t.Fatalf("%s: roll up hour %d: %v", mode, k, err)
			}
			if rep.Snapshots != 12 || rep.Queryids != fixtureQueryIDs || rep.EpochBreaks != 0 {
				t.Fatalf("%s hour %d report = %+v", mode, k, rep)
			}
			rows := readHourRows(t, p.Monitored, hour)
			checkReconciled(t, rows, k)
			got[mode] = append(got[mode], rows)
			checkCoverage(t, p.Monitored, hour, k, rows)
		}
		if mode == histstore.ModeMeta {
			checkOtherDatabaseIsolated(t, p, start)
		}
	}
	for k := range 3 {
		for id, r := range got[histstore.ModeMonitored][k] {
			if m := got[histstore.ModeMeta][k][id]; m.calls != r.calls || m.exec != r.exec {
				t.Fatalf("hour %d queryid %d: monitored %+v, meta %+v", k, id, r, m)
			}
		}
	}
}

func checkCoverage(t *testing.T, db any, hour time.Time, k int, rows map[int64]hourRow) {
	t.Helper()
	c, ok := readCoverage(t, db, hour)
	if !ok {
		t.Fatalf("hour %d: no coverage row", k)
	}
	sampled := 0.0
	for _, r := range rows {
		sampled += r.exec
	}
	pairs := 12
	if k == 0 {
		pairs = 11
	}
	if c.snapshots != 12 || c.queryids != fixtureQueryIDs || c.breaks != 0 ||
		c.source != "recorded" || c.dbExec == nil || *c.dbExec != 1000*float64(pairs) ||
		c.dealloc == nil || *c.dealloc != 0 || math.Abs(c.sampledExec-sampled) > 1e-6 {
		t.Fatalf("hour %d coverage = %+v (db %v, dealloc %v), sampled %v", k, c,
			c.dbExec, c.dealloc, sampled)
	}
}

// checkOtherDatabaseIsolated rolls the other database's noise hour in the
// meta database: its rows are its own and ours are untouched.
func checkOtherDatabaseIsolated(t *testing.T, p *histfixture.Pair, hour time.Time) {
	t.Helper()
	other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	before := readHourRows(t, p.Monitored, hour)
	if _, err := RollupHour(context.Background(), other, hour); err != nil {
		t.Fatalf("roll up the other database: %v", err)
	}
	if theirs := readHourRows(t, other, hour); len(theirs) != 0 {
		t.Fatalf("the other database's single snapshot has no pair, got %d rows", len(theirs))
	}
	if c, ok := readCoverage(t, other, hour); !ok || c.snapshots != 1 {
		t.Fatalf("other database coverage = %+v, %v; want its own single snapshot", c, ok)
	}
	after := readHourRows(t, p.Monitored, hour)
	if len(after) != len(before) || after[1].calls != before[1].calls {
		t.Fatalf("rolling another database changed ours: %d -> %d rows", len(before),
			len(after))
	}
}

func TestRollupHour_EmptyHourAndEmptyList(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	empty := pastHour(4)
	rep, err := RollupHour(ctx, p.Monitored, empty)
	if err != nil || !rep.Empty() || rep.Queryids != 0 {
		t.Fatalf("empty hour: %+v, %v", rep, err)
	}
	c, ok := readCoverage(t, p.Monitored, empty)
	if !ok || c.snapshots != 0 || c.queryids != 0 || c.dbExec != nil ||
		c.source != "counters_only" {
		t.Fatalf("empty hour coverage = %+v, %v", c, ok)
	}
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	for i := range 3 {
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i)*time.Minute), nil,
			sys{epoch: &epoch, exec: 10, calls: 1})
	}
	if rep, err = RollupHour(ctx, p.Monitored, hour); err != nil || rep.Empty() {
		t.Fatalf("empty list hour: %+v, %v", rep, err)
	}
	c, _ = readCoverage(t, p.Monitored, hour)
	if c.snapshots != 3 || c.queryids != 0 || len(readHourRows(t, p.Monitored, hour)) != 0 {
		t.Fatalf("empty list coverage = %+v", c)
	}
}

func TestRollupHour_LegacySnapshotsAreCountersOnly(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	hour := pastHour(3)
	w := snapstore.NewWriter()
	for i := range 4 {
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i)*10*time.Minute),
			[]qc{fixtureCounters(1, i)}, sys{})
	}
	if _, err := RollupHour(context.Background(), p.Monitored, hour); err != nil {
		t.Fatal(err)
	}
	c, _ := readCoverage(t, p.Monitored, hour)
	r := readHourRows(t, p.Monitored, hour)[1]
	want := sub(fixtureCounters(1, 3), fixtureCounters(1, 0))
	if c.source != "counters_only" || c.dbExec != nil || r.calls != want.calls || r.brk {
		t.Fatalf("legacy: coverage %+v, row %+v, want calls %d", c, r, want.calls)
	}
}

func TestRollupHour_PairBoundaries(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	ctx := context.Background()
	hour, epoch := pastHour(4), pastHour(20)
	end := hour.Add(30 * time.Minute)
	w := snapstore.NewWriter()
	s := sys{epoch: &epoch}
	persistCycle(t, w, p.Monitored, hour.Add(-3*time.Hour), []qc{{id: 3, calls: 1}}, s)
	persistCycle(t, w, p.Monitored, end.Add(-AnchorLookback-time.Second),
		[]qc{{id: 2, calls: 1}}, s)
	persistCycle(t, w, p.Monitored, end.Add(-AnchorLookback), []qc{{id: 1, calls: 1}}, s)
	persistCycle(t, w, p.Monitored, hour.Add(-time.Minute), []qc{{id: 4, calls: 10}}, s)
	persistCycle(t, w, p.Monitored, hour.Add(time.Minute), []qc{{id: 4, calls: 15}}, s)
	persistCycle(t, w, p.Monitored, end, []qc{{id: 1, calls: 9}, {id: 2, calls: 9},
		{id: 3, calls: 9}, {id: 4, calls: 16}}, s)
	for _, h := range []time.Time{hour.Add(-time.Hour), hour} {
		if _, err := RollupHour(ctx, p.Monitored, h); err != nil {
			t.Fatal(err)
		}
	}
	got := readHourRows(t, p.Monitored, hour)
	if got[1].calls != 8 {
		t.Fatalf("snapshots exactly AnchorLookback apart must count: %+v", got[1])
	}
	if _, ok := got[2]; ok {
		t.Fatalf("one second past AnchorLookback must not count: %+v", got[2])
	}
	if _, ok := got[3]; ok {
		t.Fatalf("a queryid gone for 3 hours contributes nothing for the gap: %+v", got[3])
	}
	if got[4].calls != 6 || got[4].pairs != 2 {
		t.Fatalf("the straddling pair counts in the later hour: %+v", got[4])
	}
	if prev := readHourRows(t, p.Monitored, hour.Add(-time.Hour)); len(prev) != 0 {
		t.Fatalf("the earlier hour must hold no pair ending after it: %+v", prev)
	}
}

func TestRollupHour_AdmittedCapGivesOneRowPerAdmittedQuery(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollup(t, p)
	hour, epoch := pastHour(3), pastHour(20)
	w := snapstore.NewWriter()
	for i := range 2 {
		qs := make([]qc, 500) // the collector admitted 500; the 501st is not in it
		for j := range qs {
			qs[j] = fixtureCounters(int64(j+1), i)
		}
		persistCycle(t, w, p.Monitored, hour.Add(time.Duration(i)*time.Minute), qs,
			sys{epoch: &epoch})
	}
	rep, err := RollupHour(context.Background(), p.Monitored, hour)
	if err != nil {
		t.Fatal(err)
	}
	rows := readHourRows(t, p.Monitored, hour)
	if rep.Queryids != 500 || len(rows) != 500 || rows[500].calls != 5000 {
		t.Fatalf("cap: report %+v, %d rows, row 500 = %+v", rep, len(rows), rows[500])
	}
	if _, ok := rows[501]; ok {
		t.Fatal("queryid 501 was never admitted and must have no row")
	}
}
