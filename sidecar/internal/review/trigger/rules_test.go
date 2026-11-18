package trigger

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests here: the rules are pure functions of their
// arguments (the queue's concurrency is in trigger_db_test.go).

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func trig(id int64, kind Kind, at time.Time) Trigger {
	return Trigger{ID: id, Database: "shop", Kind: kind, Key: "k", FiredAt: at,
		State: StateFired}
}

func ids(ts []Trigger) []int64 {
	out := make([]int64, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

// review-04b added fleet_outlier as the eighth kind.
func TestKinds_AreTheEightAndOnlyThoseAreValid(t *testing.T) {
	want := []Kind{KindEnrollment, KindPreUpgrade, KindUnresolvedRegression,
		KindCostGrowth, KindLargeMigration, KindInstanceChange, KindCalendar,
		KindFleetOutlier}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k || !k.Valid() {
			t.Errorf("kind %d = %q (valid %v), want valid %q", i, got[i], got[i].Valid(), k)
		}
	}
	for _, bad := range []Kind{"", "Calendar", "nightly", "calendar ", "pre-upgrade"} {
		if bad.Valid() {
			t.Errorf("kind %q is valid", bad)
		}
	}
	if KindCalendar.Event() || !KindPreUpgrade.Event() || !KindEnrollment.Event() {
		t.Error("only the calendar kind is not an event")
	}
}

func TestDue_EmptyQueueGivesNoRun(t *testing.T) {
	for _, pending := range [][]Trigger{nil, {}} {
		if g, due := Due(pending, 24*time.Hour, t0); due || len(g) != 0 {
			t.Fatalf("Due(%v) = %v %v, want nothing", pending, g, due)
		}
	}
}

// Boundary: a trigger 23h59m after the group's first joins it; one 24h
// after opens the next group, and so the next run.
func TestDue_CoalescesInsideTheIntervalAndSplitsAtIt(t *testing.T) {
	iv := 24 * time.Hour
	a := trig(1, KindUnresolvedRegression, t0)
	near := trig(2, KindCostGrowth, t0.Add(23*time.Hour+59*time.Minute))
	g, due := Due([]Trigger{a, near}, iv, t0.Add(iv))
	if !due || len(g) != 2 || g[0].ID != 1 || g[1].ID != 2 {
		t.Fatalf("23h59m apart: %v due %v, want one run of both", ids(g), due)
	}
	far := trig(3, KindCostGrowth, t0.Add(iv))
	g, due = Due([]Trigger{a, far}, iv, t0.Add(iv))
	if !due || len(g) != 1 || g[0].ID != 1 {
		t.Fatalf("24h apart: %v due %v, want a run of the first alone", ids(g), due)
	}
	// The second opens its own group, due a full interval after it fired.
	if g, due = Due([]Trigger{far}, iv, t0.Add(2*iv-time.Second)); due {
		t.Fatalf("second group due early: %v", ids(g))
	}
	if g, due = Due([]Trigger{far}, iv, t0.Add(2*iv)); !due || len(g) != 1 || g[0].ID != 3 {
		t.Fatalf("second group: %v due %v, want its own run", ids(g), due)
	}
}

func TestDue_WaitsForTheIntervalToClose(t *testing.T) {
	a := trig(1, KindEnrollment, t0)
	if g, due := Due([]Trigger{a}, 24*time.Hour, t0.Add(24*time.Hour-time.Minute)); due {
		t.Fatalf("due before the interval closed: %v", ids(g))
	}
}

// pre_upgrade bypasses the interval and takes every trigger already fired.
func TestDue_PreUpgradeBypassesTheInterval(t *testing.T) {
	a := trig(1, KindCalendar, t0)
	b := trig(2, KindPreUpgrade, t0.Add(30*time.Hour))
	g, due := Due([]Trigger{a, b}, 24*time.Hour, t0.Add(30*time.Hour))
	if !due || len(g) != 2 {
		t.Fatalf("pre_upgrade: %v due %v, want an immediate run of both", ids(g), due)
	}
	g, due = Due([]Trigger{trig(5, KindPreUpgrade, t0)}, 24*time.Hour, t0)
	if !due || len(g) != 1 {
		t.Fatalf("lone pre_upgrade: %v due %v, want an immediate run", ids(g), due)
	}
}

// Invalid input: a zero or negative interval is not a reason to run
// every tick; it reads as no coalescing window at all only for due-ness.
func TestDue_ZeroIntervalRunsAtOnceWithTheOldest(t *testing.T) {
	a, b := trig(1, KindCalendar, t0), trig(2, KindCostGrowth, t0.Add(time.Hour))
	g, due := Due([]Trigger{a, b}, 0, t0.Add(time.Hour))
	if !due || len(g) != 1 || g[0].ID != 1 {
		t.Fatalf("zero interval: %v due %v, want the oldest alone", ids(g), due)
	}
}

func TestCalendarDue_Boundary(t *testing.T) {
	last := t0
	if CalendarDue(last, last.Add(29*24*time.Hour+23*time.Hour), 30) {
		t.Error("calendar fired at 29 days 23 hours")
	}
	if !CalendarDue(last, last.Add(30*24*time.Hour), 30) {
		t.Error("calendar did not fire at 30 days")
	}
	if CalendarDue(time.Time{}, t0, 30) {
		t.Error("calendar fired with no full review yet (enrollment's job)")
	}
	if CalendarDue(last, last.Add(400*24*time.Hour), 0) {
		t.Error("calendar fired with calendar_days 0")
	}
}

func TestEnrollmentReady_FourteenDaysAtEightyPercent(t *testing.T) {
	now := t0
	oldest := now.Add(-EnrollmentWindow)
	full := int(EnrollmentWindow / time.Hour)
	if full != 336 {
		t.Fatalf("window hours = %d, want 336 (two weekly cycles)", full)
	}
	if EnrollmentReady(265, oldest, now) { // 78.9%
		t.Error("enrolled at 79% coverage")
	}
	if !EnrollmentReady(269, oldest, now) { // 80.06%
		t.Error("not enrolled at 80% coverage")
	}
	if EnrollmentReady(268, oldest, now) { // 79.76%
		t.Error("enrolled at 268 of 336 hours (under 80%)")
	}
	// 13 days of complete rows: full coverage of what exists, too young.
	if EnrollmentReady(312, now.Add(-13*24*time.Hour), now) {
		t.Error("enrolled with only 13 days of rollups")
	}
	if EnrollmentReady(0, time.Time{}, now) || EnrollmentReady(-5, oldest, now) {
		t.Error("enrolled with no rollups")
	}
}

func sizes(start time.Time, vals ...int64) []DaySize {
	out := make([]DaySize, len(vals))
	for i, v := range vals {
		out[i] = DaySize{Day: start.AddDate(0, 0, i), Bytes: v}
	}
	return out
}

func steady(days int, step int64) []int64 {
	out := make([]int64, days)
	for i := range out {
		out[i] = 100<<30 + int64(i)*step + int64(i%3)*(8<<20)
	}
	return out
}

func TestGrowthAnomaly_SpikeOverZFourFires(t *testing.T) {
	vals := steady(15, 1<<30)
	vals = append(vals, vals[len(vals)-1]+20<<30) // 20 GiB in a day
	a, ok := GrowthAnomaly(sizes(t0, vals...))
	if !ok || a.Z < 4 || a.GrowthBytes != 20<<30 ||
		!a.Day.Equal(t0.AddDate(0, 0, 15)) {
		t.Fatalf("anomaly %+v %v, want z >= 4 on day 15 with 20 GiB", a, ok)
	}
}

func TestGrowthAnomaly_SteadyGrowthAndShortSeriesDoNotFire(t *testing.T) {
	if a, ok := GrowthAnomaly(sizes(t0, steady(20, 1<<30)...)); ok {
		t.Fatalf("steady growth fired: %+v", a)
	}
	short := steady(5, 1<<30)
	short = append(short, short[4]+50<<30)
	if a, ok := GrowthAnomaly(sizes(t0, short...)); ok {
		t.Fatalf("5 days of history fired: %+v", a)
	}
	if _, ok := GrowthAnomaly(nil); ok {
		t.Fatal("no sizes fired")
	}
	// A small database's noise stays under the floor however large z is.
	tiny := []int64{1000, 1001, 1002, 1003, 1004, 1005, 1006, 1007, 1008, 1009, 9000000}
	if a, ok := GrowthAnomaly(sizes(t0, tiny...)); ok {
		t.Fatalf("growth under the floor fired: %+v", a)
	}
}

// A missing day breaks the series: no day-over-day growth across a gap.
func TestGrowthAnomaly_GapIsNotGrowth(t *testing.T) {
	ss := sizes(t0, steady(15, 1<<30)...)
	ss = append(ss, DaySize{Day: t0.AddDate(0, 0, 20), Bytes: ss[14].Bytes + 20<<30})
	if a, ok := GrowthAnomaly(ss); ok {
		t.Fatalf("growth across a 6-day gap fired: %+v", a)
	}
}

func TestInstanceChanged_OnlyKnownFactsCount(t *testing.T) {
	prev := Facts{Class: "db.r6g.large", VCPUs: 2, MemoryBytes: 16 << 30}
	cases := []struct {
		cur  Facts
		want bool
	}{
		{prev, false},
		{Facts{Class: "db.r6g.xlarge", VCPUs: 4, MemoryBytes: 32 << 30}, true},
		{Facts{Class: "db.r6g.large", VCPUs: 2, MemoryBytes: 32 << 30}, true},
		{Facts{Class: "", VCPUs: 0, MemoryBytes: 0}, false},
		{Facts{Class: "db.r6g.large", VCPUs: 0, MemoryBytes: 16 << 30}, false},
		{Facts{Class: "db.r6g.large", VCPUs: 8, MemoryBytes: 16 << 30}, true},
	}
	for _, c := range cases {
		if got := InstanceChanged(prev, c.cur); got != c.want {
			t.Errorf("InstanceChanged(%+v, %+v) = %v, want %v", prev, c.cur, got, c.want)
		}
	}
	if InstanceChanged(Facts{}, prev) {
		t.Error("a first observation counted as a change")
	}
}

func TestLargeTables_StrictlyLargerThanTheThreshold(t *testing.T) {
	const gib = int64(1 << 30)
	in := []TableSize{{Table: "public.a", Bytes: 10 * gib}, {Table: "public.b",
		Bytes: 10*gib + 1}, {Table: "public.c", Bytes: 0}, {Table: "", Bytes: 99 * gib}}
	got := LargeTables(in, 10*gib)
	if len(got) != 1 || got[0].Table != "public.b" {
		t.Fatalf("LargeTables = %+v, want public.b alone", got)
	}
	if got := LargeTables(in, 0); len(got) != 0 {
		t.Fatalf("threshold 0 = %+v, want nothing (unset threshold never fires)", got)
	}
	if got := LargeTables(nil, gib); len(got) != 0 {
		t.Fatalf("no tables = %+v", got)
	}
}

func TestMigrationKey_IsStableBoundedAndOrderFree(t *testing.T) {
	a := []TableSize{{Table: "public.a", Bytes: 1}, {Table: "public.b", Bytes: 2}}
	b := []TableSize{{Table: "public.b", Bytes: 2}, {Table: "public.a", Bytes: 1}}
	ka, kb := migrationKey(a, t0), migrationKey(b, t0)
	if ka != kb || !strings.HasPrefix(ka, "migration:") || len(ka) > 300 {
		t.Fatalf("keys %q %q, want equal, bounded and prefixed", ka, kb)
	}
	if migrationKey(a, t0.AddDate(0, 0, 1)) == ka {
		t.Fatal("a migration on another day kept the key (one trigger per day)")
	}
}
