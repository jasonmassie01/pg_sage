package querystore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01b on a real server, in both history placements: the daily tier
// sums a UTC day's hourly rows per queryid, counts the plan fingerprints
// and the hours with an epoch break, never shrinks a row from a partly
// purged day, and the worker rolls each closed day once its hours are.

// hourlyFix is one sage.query_rollup_hourly row written by a test.
type hourlyFix struct {
	qid   int64
	hour  time.Time
	calls int64
	exec  float64
	plan  *string
	brk   bool
}

func insertHourly(t testing.TB, db any, f hourlyFix) {
	t.Helper()
	_, err := histstore.Resolve(db).Exec(context.Background(), `INSERT INTO
		sage.query_rollup_hourly (queryid, hour, calls, exec_ms, rows, shared_blks_hit,
		shared_blks_read, temp_blks_read, temp_blks_written, wal_bytes, plan_hash,
		epoch_break, pairs{dbcol}) VALUES ($1, $2, $3::int8, $4, $3::int8 * 3,
		$3::int8 * 7, $3::int8, 1, 2,
		$3::int8 * 100, $5, $6, 11{dbval})`, f.qid, f.hour, f.calls, f.exec, f.plan, f.brk)
	if err != nil {
		t.Fatalf("insert hourly row %+v: %v", f, err)
	}
}

// dailyRow is one sage.query_rollup_daily row.
type dailyRow struct {
	calls, rows, hit, read, tread, twrite int64
	wal                                   *int64
	exec                                  float64
	hours, breaks, plans                  int16
	lastPlan                              *string
}

func readDaily(t testing.TB, db any, qid int64, day time.Time) (dailyRow, bool) {
	t.Helper()
	var r dailyRow
	err := histstore.Resolve(db).QueryRow(context.Background(), `SELECT d.calls, d.rows,
		d.shared_blks_hit, d.shared_blks_read, d.temp_blks_read, d.temp_blks_written,
		d.wal_bytes, d.exec_ms, d.hours, d.epoch_breaks, d.plan_hashes, d.last_plan_hash
		FROM sage.query_rollup_daily d WHERE {db:d} AND d.queryid = $1 AND d.day = $2::date`,
		qid, day.Format(time.DateOnly)).Scan(&r.calls, &r.rows, &r.hit, &r.read, &r.tread,
		&r.twrite, &r.wal, &r.exec, &r.hours, &r.breaks, &r.plans, &r.lastPlan)
	if err != nil {
		return dailyRow{}, false
	}
	return r, true
}

func dailyCount(t testing.TB, db any) int {
	t.Helper()
	var n int
	if err := histstore.Resolve(db).QueryRow(context.Background(), `SELECT count(*)
		FROM sage.query_rollup_daily d WHERE {db:d}`).Scan(&n); err != nil {
		t.Fatalf("count daily rows: %v", err)
	}
	return n
}

// pastDay is the UTC day n days before today.
func pastDay(n int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n)
}

func truncateAllRollup(t testing.TB, p *histfixture.Pair) {
	t.Helper()
	truncateRollup(t, p)
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(),
			"TRUNCATE sage.query_rollup_daily"); err != nil {
			t.Fatalf("truncate daily rollup: %v", err)
		}
	}
}

// writeTwoDays writes 48 hourly rows of queryid 5 (the plan flips from p1
// to p2 at hour 36, hour 30 has an epoch break) and 10 rows of queryid 6
// without a plan fingerprint, from day0.
func writeTwoDays(t *testing.T, db any, day0 time.Time) {
	t.Helper()
	p1, p2 := "p1", "p2"
	for h := range 48 {
		plan := &p1
		if h >= 36 {
			plan = &p2
		}
		insertHourly(t, db, hourlyFix{qid: 5, hour: day0.Add(time.Duration(h) * time.Hour),
			calls: int64(h + 1), exec: float64(h) + 0.5, plan: plan, brk: h == 30})
	}
	for h := range 10 {
		insertHourly(t, db, hourlyFix{qid: 6, hour: day0.Add(time.Duration(h) * time.Hour),
			calls: 2, exec: 1})
	}
}

func checkDailyRow(t *testing.T, mode histstore.Mode, got dailyRow, calls int64, exec float64,
	hours, breaks, plans int16, last string) {
	t.Helper()
	if got.calls != calls || got.rows != 3*calls || got.hit != 7*calls ||
		got.read != calls || got.tread != int64(hours) || got.twrite != 2*int64(hours) ||
		got.wal == nil || *got.wal != 100*calls || got.exec != exec ||
		got.hours != hours || got.breaks != breaks || got.plans != plans {
		t.Fatalf("%s: daily row %+v, want calls %d exec %v hours %d breaks %d plans %d",
			mode, got, calls, exec, hours, breaks, plans)
	}
	if (last == "") != (got.lastPlan == nil) || (got.lastPlan != nil && *got.lastPlan != last) {
		t.Fatalf("%s: last plan %v, want %q", mode, got.lastPlan, last)
	}
}

func TestRollupDay_SumsTheHoursInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	day0 := pastDay(3)
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateAllRollup(t, p)
		writeTwoDays(t, p.Monitored, day0)
		if mode == histstore.ModeMeta {
			other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
			if err != nil {
				t.Fatal(err)
			}
			insertHourly(t, other, hourlyFix{qid: 5, hour: day0, calls: 1_000_000, exec: 9e9})
		}
		for d := range 2 {
			rep, err := RollupDay(ctx, p.Monitored, day0.AddDate(0, 0, d))
			if err != nil {
				t.Fatalf("%s: roll day %d: %v", mode, d, err)
			}
			wantIDs := 2 - d
			if rep.Queryids != wantIDs || !rep.Day.Equal(day0.AddDate(0, 0, d)) {
				t.Fatalf("%s: day %d report %+v, want %d queryids", mode, d, rep, wantIDs)
			}
		}
		got0, ok0 := readDaily(t, p.Monitored, 5, day0)
		got1, ok1 := readDaily(t, p.Monitored, 5, day0.AddDate(0, 0, 1))
		got6, ok6 := readDaily(t, p.Monitored, 6, day0)
		if !ok0 || !ok1 || !ok6 || dailyCount(t, p.Monitored) != 3 {
			t.Fatalf("%s: daily rows %v %v %v, count %d, want 3", mode, ok0, ok1, ok6,
				dailyCount(t, p.Monitored))
		}
		// day 0: hours 0-23, calls 1..24 = 300, exec 0.5..23.5 = 288
		checkDailyRow(t, mode, got0, 300, 288, 24, 0, 1, "p1")
		// day 1: hours 24-47, calls 25..48 = 876, exec 24.5..47.5 = 864, one break,
		// the plan flipped once (p1 then p2)
		checkDailyRow(t, mode, got1, 876, 864, 24, 1, 2, "p2")
		checkDailyRow(t, mode, got6, 20, 10, 10, 0, 0, "")
	}
}

func TestRollupDay_InvalidDay(t *testing.T) {
	ctx := context.Background()
	if _, err := RollupDay(ctx, nil, time.Time{}); !errors.Is(err, ErrRollupDay) {
		t.Fatalf("zero day: err = %v, want ErrRollupDay", err)
	}
	today := time.Now().UTC()
	if _, err := RollupDay(ctx, nil, today); !errors.Is(err, ErrRollupDay) ||
		!strings.Contains(err.Error(), today.Format(time.DateOnly)) {
		t.Fatalf("today (not over): err = %v, want ErrRollupDay naming the day", err)
	}
	if _, err := RollupDay(ctx, nil, today.AddDate(0, 0, 2)); !errors.Is(err, ErrRollupDay) {
		t.Fatalf("a future day: err = %v, want ErrRollupDay", err)
	}
}

func TestRollupDay_EmptyAndPartlyPurgedDaysKeepTheirRows(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	p.Switch(t, histstore.ModeMonitored)
	truncateAllRollup(t, p)
	day0 := pastDay(4)
	rep, err := RollupDay(ctx, p.Monitored, day0)
	if err != nil || rep.Queryids != 0 || dailyCount(t, p.Monitored) != 0 {
		t.Fatalf("a day without hourly rows: report %+v err %v, want nothing written", rep,
			err)
	}
	writeTwoDays(t, p.Monitored, day0)
	if _, err := RollupDay(ctx, p.Monitored, day0); err != nil {
		t.Fatal(err)
	}
	before, _ := readDaily(t, p.Monitored, 5, day0)
	// The purge removed the day's first twelve hours: a re-roll must not
	// shrink the row it can no longer rebuild.
	if _, err := p.Monitored.Exec(ctx, `DELETE FROM sage.query_rollup_hourly
		WHERE hour < $1`, day0.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := RollupDay(ctx, p.Monitored, day0); err != nil {
		t.Fatal(err)
	}
	if after, _ := readDaily(t, p.Monitored, 5, day0); !reflect.DeepEqual(after, before) {
		t.Fatalf("a partly purged day re-rolled to %+v, want %+v kept", after, before)
	}
	if _, ok := readDaily(t, p.Monitored, 6, day0); !ok {
		t.Fatal("a queryid whose hours are all purged lost its daily row")
	}
	// A late snapshot re-rolled an hour that is still kept: the day follows.
	if _, err := p.Monitored.Exec(ctx, `UPDATE sage.query_rollup_hourly SET calls = calls + 10
		WHERE queryid = 5 AND hour = $1`, day0.Add(30*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := RollupDay(ctx, p.Monitored, day0.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := readDaily(t, p.Monitored, 5, day0.AddDate(0, 0, 1)); got.calls != 886 {
		t.Fatalf("re-rolled day 1 calls %d, want 886 (876 + the late 10)", got.calls)
	}
}

func TestRollupDay_FailureNamesTheDayAndTheSource(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rollup_day_err"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.query_rollup_hourly
		RENAME TO query_rollup_hourly_moved`); err != nil {
		t.Fatal(err)
	}
	day := pastDay(2)
	_, err = RollupDay(ctx, pool, day)
	if err == nil || !strings.Contains(err.Error(), day.Format(time.DateOnly)) ||
		!strings.Contains(err.Error(), "sage.query_rollup_hourly") {
		t.Fatalf("err = %v, want one naming %s and sage.query_rollup_hourly", err,
			day.Format(time.DateOnly))
	}
	if dailyCount(t, pool) != 0 {
		t.Fatal("a failed roll wrote daily rows")
	}
}

// fullCoverage writes a coverage row for every hour from from through to,
// so the hourly part of a worker pass has nothing left to roll.
func fullCoverage(t *testing.T, db any, from, to time.Time) {
	t.Helper()
	_, err := histstore.Resolve(db).Exec(context.Background(), `INSERT INTO
		sage.query_rollup_coverage (hour, db_exec_ms, sampled_exec_ms, sampled_queryids,
		epoch_breaks, snapshots, epoch_source{dbcol})
		SELECT h, 10, 9, 1, 0, 12, 'recorded'{dbval}
		FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 hour') h`,
		from, to)
	if err != nil {
		t.Fatalf("write coverage rows: %v", err)
	}
}

func TestRollupWorker_RollsClosedDaysBoundedPerPass(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateAllRollup(t, p)
		now := time.Now().UTC()
		first := pastDay(11)
		for d := range 10 { // days d-11 .. d-2: all closed and due
			insertHourly(t, p.Monitored, hourlyFix{qid: 9,
				hour: first.AddDate(0, 0, d).Add(5 * time.Hour), calls: int64(d + 1), exec: 1})
		}
		fullCoverage(t, p.Monitored, first, lastDueHour(now))
		w := &RollupWorker{Store: p.Monitored, Database: "day-" + string(mode),
			Clock: func() time.Time { return now }}
		if _, err := w.Pass(ctx); err != nil {
			t.Fatalf("%s: first pass: %v", mode, err)
		}
		if n := dailyCount(t, p.Monitored); n != dailyDaysPerPass {
			t.Fatalf("%s: first pass rolled %d days, want %d", mode, n, dailyDaysPerPass)
		}
		if _, err := w.Pass(ctx); err != nil {
			t.Fatalf("%s: second pass: %v", mode, err)
		}
		if n := dailyCount(t, p.Monitored); n != 10 {
			t.Fatalf("%s: after two passes %d days, want 10", mode, n)
		}
		if got, ok := readDaily(t, p.Monitored, 9, first.AddDate(0, 0, 9)); !ok ||
			got.calls != 10 {
			t.Fatalf("%s: newest day %+v, want calls 10", mode, got)
		}
		if _, err := w.Pass(ctx); err != nil || dailyCount(t, p.Monitored) != 10 {
			t.Fatalf("%s: a caught-up pass changed the daily tier (err %v)", mode, err)
		}
	}
}

func TestRollupWorker_DayWaitsForItsLastHour(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	p.Switch(t, histstore.ModeMonitored)
	truncateAllRollup(t, p)
	day := pastDay(3)
	insertHourly(t, p.Monitored, hourlyFix{qid: 9, hour: day.Add(2 * time.Hour), calls: 1,
		exec: 1})
	// Coverage stops at 22:00: hour 23 is not rolled yet, so the day is not.
	fullCoverage(t, p.Monitored, day, day.Add(22*time.Hour))
	w := &RollupWorker{Store: p.Monitored, Database: "day-wait",
		Clock: func() time.Time { return day.AddDate(0, 0, 2) }}
	if _, err := w.rollDays(ctx); err != nil {
		t.Fatal(err)
	}
	if n := dailyCount(t, p.Monitored); n != 0 {
		t.Fatalf("a day whose 23:00 hour is not rolled was rolled (%d rows)", n)
	}
	fullCoverage(t, p.Monitored, day.Add(23*time.Hour), day.Add(23*time.Hour))
	if _, err := w.rollDays(ctx); err != nil || dailyCount(t, p.Monitored) != 1 {
		t.Fatalf("once 23:00 is rolled the day must be (err %v)", err)
	}
}
