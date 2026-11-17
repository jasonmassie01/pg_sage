package capacity

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// livePool is a fresh database with the sage schema, bootstrapped twice
// (the second run must be a clean no-op).
func livePool(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for i := 0; i < 2; i++ {
		if err := schema.Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", i+1, err)
		}
	}
	return pool, ctx
}

// The fixture's statements: one application query and one of pg_sage's.
const (
	appQueryID  = 101
	selfQueryID = 202
)

// cycle writes one queries and one system snapshot at at, as the
// collector does: appCalls and selfCalls are the cumulative counters.
func cycle(t *testing.T, ctx context.Context, w *snapstore.Writer, pool *pgxpool.Pool,
	at, epoch time.Time, appCalls, selfCalls int64) {
	t.Helper()
	q := func(id, calls int64, text string) map[string]any {
		return map[string]any{"queryid": id, "query": text, "calls": calls,
			"total_exec_time": float64(calls), "mean_exec_time": 1.0, "rows": calls,
			"shared_blks_hit": 0, "shared_blks_read": 0, "shared_blks_dirtied": 0,
			"shared_blks_written": 0, "temp_blks_read": 0, "temp_blks_written": 0,
			"blk_read_time": 0.0, "blk_write_time": 0.0, "wal_bytes": 0}
	}
	queries, err := json.Marshal([]map[string]any{
		q(appQueryID, appCalls, "SELECT * FROM orders WHERE id = $1"),
		q(selfQueryID, selfCalls, "/* pg_sage */ SELECT count(*) FROM pg_stat_activity")})
	if err != nil {
		t.Fatal(err)
	}
	system, err := json.Marshal(map[string]any{"active_backends": 1, "total_backends": 2,
		"stat_statements_usage":    map[string]any{"dealloc": 0},
		"statements_epoch":         epoch.Format(time.RFC3339Nano),
		"statements_total_exec_ms": float64(appCalls + selfCalls),
		"statements_calls":         appCalls + selfCalls, "statements_entries": 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Persist(ctx, pool, at, []snapstore.Row{{Category: "queries",
		Data: queries}, {Category: "system", Data: system}}); err != nil {
		t.Fatalf("persist the cycle at %s: %v", at, err)
	}
}

// seedDays writes n days ending yesterday (UTC), two cycles at 12:00 and
// 12:30 each, and rolls each day's hour 12 up through evid-01's rollup.
// The application's counter moves only on the first active days; pg_sage's
// own moves every cycle. It returns the first day.
func seedDays(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n, active int) time.Time {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -n)
	epoch := first.AddDate(0, 0, -1)
	w := snapstore.NewWriter()
	var app, self int64
	for d := range n {
		noon := first.AddDate(0, 0, d).Add(12 * time.Hour)
		for k := range 2 {
			if d < active {
				app += 50
			}
			self += 7
			cycle(t, ctx, w, pool, noon.Add(time.Duration(k)*30*time.Minute), epoch, app,
				self)
		}
		if _, err := querystore.RollupHour(ctx, pool, noon); err != nil {
			t.Fatalf("roll up %s: %v", noon, err)
		}
	}
	return first
}

func TestDBCallSource_PgSageOwnStatementsLeaveADatabaseQuiet(t *testing.T) {
	pool, ctx := livePool(t, "capacity_idle")
	first := seedDays(t, ctx, pool, 20, 4)
	// The oldest 8 days move to the daily tier: their hourly rows and
	// coverage hours go, as retention removes them.
	for d := range 8 {
		day := first.AddDate(0, 0, d)
		if _, err := querystore.RollupDay(ctx, pool, day); err != nil {
			t.Fatalf("roll up day %s: %v", day, err)
		}
	}
	cut := first.AddDate(0, 0, 8)
	for _, table := range []string{"query_rollup_hourly", "query_rollup_coverage"} {
		if _, err := pool.Exec(ctx, "DELETE FROM sage."+table+" WHERE hour < $1",
			cut); err != nil {
			t.Fatal(err)
		}
	}
	days, err := NewDBCallSource(pool).Read(ctx, first, first.AddDate(0, 0, 20))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(days) != 20 {
		t.Fatalf("%d days, want 20: %+v", len(days), days)
	}
	for i, d := range days {
		if !d.Day.Equal(first.AddDate(0, 0, i)) || !d.Observed {
			t.Fatalf("day %d = %+v, want %s observed", i, d, first.AddDate(0, 0, i))
		}
		if wantCalls := i < 4; (d.WorkloadCalls > 0) != wantCalls {
			t.Fatalf("day %d workload calls %d (pg_sage's own must not count)", i,
				d.WorkloadCalls)
		}
	}
	if got := QuietDays(days); got != 16 || !Idle(days) {
		t.Fatalf("QuietDays %d, want 16 (the application stopped after day 3)", got)
	}
}

func TestDBCallSource_AnApplicationCallKeepsItActive(t *testing.T) {
	pool, ctx := livePool(t, "capacity_active")
	first := seedDays(t, ctx, pool, 20, 20)
	days, err := NewDBCallSource(pool).Read(ctx, first, first.AddDate(0, 0, 20))
	if err != nil {
		t.Fatal(err)
	}
	if QuietDays(days) != 0 || Idle(days) {
		t.Fatalf("a database called every day has %d quiet days", QuietDays(days))
	}
}

func TestDBCallSource_EmptyHistoryIsUnobserved(t *testing.T) {
	pool, ctx := livePool(t, "capacity_empty")
	to := time.Now().UTC().Truncate(24 * time.Hour)
	days, err := NewDBCallSource(pool).Read(ctx, to.AddDate(0, 0, -20), to)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 20 || Idle(days) {
		t.Fatalf("an empty history: %d days, idle %v; want 20 unobserved", len(days),
			Idle(days))
	}
	for _, d := range days {
		if d.Observed || d.WorkloadCalls != 0 {
			t.Fatalf("day %+v observed with no history", d)
		}
	}
}

func TestDBCallSource_ReadErrorNamesTheRead(t *testing.T) {
	pool, ctx := livePool(t, "capacity_readerr")
	if _, err := pool.Exec(ctx, "DROP TABLE sage.query_rollup_daily CASCADE"); err != nil {
		t.Fatal(err)
	}
	to := time.Now().UTC().Truncate(24 * time.Hour)
	_, err := NewDBCallSource(pool).Read(ctx, to.AddDate(0, 0, -20), to)
	if err == nil || !containsAll(err.Error(), "workload calls", "query_rollup_daily") {
		t.Fatalf("err %v, want the failed read named", err)
	}
}

func TestForecastLog_TheErrorIsWrittenOnceWhenTheThirtyDayMarkPasses(t *testing.T) {
	pool, ctx := livePool(t, "capacity_forecast")
	log := NewForecastLog(pool)
	p, err := Project(series(dbID, "", linear(28, 100*gib, gib, 0)), window.From)
	if err != nil {
		t.Fatal(err)
	}
	made := p.Origin
	stored, err := log.Record(ctx, "app", p)
	if err != nil || !stored {
		t.Fatalf("record: %v, %v", stored, err)
	}
	if again, err := log.Record(ctx, "app", p); err != nil || again {
		t.Fatalf("a second record of the same review day: %v, %v", again, err)
	}
	before := []DaySize{{Day: made.AddDate(0, 0, 29), Bytes: 150 * gib}}
	got, err := log.Settle(ctx, "app", before)
	if err != nil || len(got) != 0 {
		t.Fatalf("settled before the mark: %+v, %v", got, err)
	}
	at := []DaySize{{Day: made.AddDate(0, 0, 30), Bytes: 150 * gib}}
	got, err = log.Settle(ctx, "app", at)
	if err != nil || len(got) != 1 {
		t.Fatalf("at the mark: %+v, %v; want one error record", got, err)
	}
	e := got[0]
	// Projected 157 GiB, measured 150: off by 7/150.
	if e.ErrorPct == nil || math.Abs(*e.ErrorPct-100*7.0/150) > 1e-6 ||
		!e.MadeOn.Equal(made) || !e.TargetOn.Equal(made.AddDate(0, 0, 30)) {
		t.Fatalf("error record %+v", e)
	}
	second := []DaySize{{Day: made.AddDate(0, 0, 30), Bytes: 999 * gib}}
	got, err = log.Settle(ctx, "app", second)
	if err != nil || len(got) != 0 {
		t.Fatalf("the second pass at the mark: %+v, %v; want a no-op", got, err)
	}
	var measured float64
	var n int
	if err := pool.QueryRow(ctx, `SELECT measured_bytes, count(*) OVER ()
		FROM sage.review_capacity_forecast WHERE database_name = 'app'`).Scan(&measured,
		&n); err != nil {
		t.Fatal(err)
	}
	if measured != 150*gib || n != 1 {
		t.Fatalf("stored %v bytes in %d rows, want the first measure kept", measured, n)
	}
	other, err := log.Settle(ctx, "other", at)
	if err != nil || len(other) != 0 {
		t.Fatalf("another database settled app's record: %+v, %v", other, err)
	}
}

func TestForecastLog_ZeroMeasuredSizeHasNoPercentage(t *testing.T) {
	pool, ctx := livePool(t, "capacity_forecast_zero")
	log := NewForecastLog(pool)
	p, err := Project(series(dbID, "", linear(10, 10*gib, gib, 0)), window.From)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Record(ctx, "app", p); err != nil {
		t.Fatal(err)
	}
	got, err := log.Settle(ctx, "app", []DaySize{{Day: p.Origin.AddDate(0, 0, 30)}})
	if err != nil || len(got) != 1 || got[0].ErrorPct != nil {
		t.Fatalf("a zero measure: %+v, %v; want a record without a percentage", got, err)
	}
}

func TestGenerate_LiveLogSettlesThenRecords(t *testing.T) {
	pool, ctx := livePool(t, "capacity_generate")
	log := NewForecastLog(pool)
	g := New(Config{Log: log})
	if _, _, err := g.Generate(ctx, steadyPack(t), testEnv()); err != nil {
		t.Fatalf("generate: %v", err)
	}
	var n int
	var target time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*), max(target_on)
		FROM sage.review_capacity_forecast WHERE database_name = 'app'`).Scan(&n,
		&target); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !target.Equal(day(27).AddDate(0, 0, 30)) {
		t.Fatalf("%d records targeting %s, want one for %s", n, target,
			day(27).AddDate(0, 0, 30))
	}
}

func containsAll(s string, parts ...string) bool {
	return hasNote([]string{s}, parts...)
}
