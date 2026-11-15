package retention

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// evid-01b: the per-query rollup ages out by tier. Hourly rows follow
// retention.query_rollup_hourly_days (35; never past the snapshots they
// are rebuilt from) and are purged only for days whose daily row exists;
// daily rows and the hourly coverage follow
// retention.query_rollup_daily_days (400), so a long window can still
// state its coverage after the hourly rows are gone.

const rollupQID = int64(424242)

func rollupRules(t *testing.T, cfg *config.Config) map[string]purgeRule {
	t.Helper()
	found := map[string]purgeRule{}
	for _, r := range purgeRules(cfg) {
		switch r.table {
		case "query_rollup_hourly", "query_rollup_daily", "query_rollup_coverage":
			if _, dup := found[r.table]; dup {
				t.Fatalf("two purge rules for sage.%s", r.table)
			}
			found[r.table] = r
		}
	}
	return found
}

func TestPurgeRules_QueryRollupTiers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Retention.SnapshotsDays = 90
	got := rollupRules(t, cfg)
	for table, want := range map[string]struct {
		col  string
		days int
	}{
		"query_rollup_hourly":   {"hour", 35},
		"query_rollup_daily":    {"day", 400},
		"query_rollup_coverage": {"hour", 400},
	} {
		r, ok := got[table]
		if !ok {
			t.Fatalf("no purge rule for sage.%s", table)
		}
		if r.timeCol != want.col || r.days != want.days || r.partitioned != nil {
			t.Fatalf("%s rule = %+v, want %s and %d days, unpartitioned", table, r,
				want.col, want.days)
		}
		if _, exempt := retentionExemptions[table]; exempt {
			t.Fatalf("sage.%s is both purged and exempt", table)
		}
	}
	if got["query_rollup_hourly"].extra == "" || got["query_rollup_daily"].extra != "" {
		t.Fatalf("only the hourly purge waits for its daily row: %+v", got)
	}
	cfg.Retention.SnapshotsDays = 20
	if d := rollupRules(t, cfg)["query_rollup_hourly"].days; d != 20 {
		t.Fatalf("snapshots_days 20: hourly rows kept %d days, want 20 (never past "+
			"their source)", d)
	}
	cfg.Retention.QueryRollupDailyDays, cfg.Retention.QueryRollupHourlyDays = 0, 0
	for table, r := range rollupRules(t, cfg) {
		if r.days != 0 {
			t.Fatalf("0 (off) must leave sage.%s unpurged, got %d days", table, r.days)
		}
	}
}

func rollupCfg() *config.Config {
	cfg := allDays(90)
	cfg.Retention.QueryRollupHourlyDays = 35
	cfg.Retention.QueryRollupDailyDays = 400
	return cfg
}

func insertRollupHour(t *testing.T, ctx context.Context, hour time.Time, calls int64) {
	t.Helper()
	execRetry(t, ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour, calls, exec_ms,
		rows, shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written,
		epoch_break, pairs) VALUES ($1, $2, $3, 1, 1, 1, 1, 0, 0, false, 1)`,
		rollupQID, hour, calls)
}

func insertDaily(t *testing.T, ctx context.Context, day time.Time) {
	t.Helper()
	execRetry(t, ctx, `INSERT INTO sage.query_rollup_daily (queryid, day, calls, exec_ms,
		rows, shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written, hours,
		epoch_breaks, plan_hashes) VALUES ($1, $2::date, 1, 1, 1, 1, 1, 0, 0, 1, 0, 0)
		ON CONFLICT DO NOTHING`, rollupQID, day.UTC().Format(time.DateOnly))
}

func cleanRollup(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, table := range []string{"query_rollup_hourly", "query_rollup_daily"} {
		execRetry(t, ctx, `DELETE FROM sage.`+table+` WHERE queryid = $1`, rollupQID)
	}
	execRetry(t, ctx, `DELETE FROM sage.query_rollup_coverage WHERE sampled_queryids = 4242`)
	t.Cleanup(func() {
		for _, table := range []string{"query_rollup_hourly", "query_rollup_daily"} {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM sage.`+table+
				` WHERE queryid = $1`, rollupQID)
		}
	})
}

func hourlyLeft(t *testing.T, ctx context.Context) int {
	return countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_hourly
		WHERE queryid = $1`, rollupQID)
}

func TestRun_HourlyRollupBoundaryAt35Days(t *testing.T) {
	_, ctx := requireDB(t)
	cleanRollup(t, ctx)
	var now time.Time
	if err := testPool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-35 * 24 * time.Hour).Truncate(time.Hour) // exactly 35 days, on the hour
	young := old.Add(time.Hour)                              // 35 days minus one hour
	insertRollupHour(t, ctx, old, 1)
	insertRollupHour(t, ctx, young, 2)
	insertDaily(t, ctx, old)
	insertDaily(t, ctx, young)
	New(testPool, rollupCfg(), noopLog).Run(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_hourly
		WHERE queryid = $1 AND calls = 2`, rollupQID); n != 1 || hourlyLeft(t, ctx) != 1 {
		t.Fatalf("hourly rows left %d (young kept %d), want only the one 35 days minus "+
			"an hour old", hourlyLeft(t, ctx), n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_daily
		WHERE queryid = $1`, rollupQID); n == 0 {
		t.Fatal("the daily rows (400 days) must survive the hourly purge")
	}
}

func TestRun_HourlyRollupWaitsForItsDailyRow(t *testing.T) {
	_, ctx := requireDB(t)
	cleanRollup(t, ctx)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -40)
	insertRollupHour(t, ctx, day.Add(3*time.Hour), 1)
	New(testPool, rollupCfg(), noopLog).Run(ctx)
	if hourlyLeft(t, ctx) != 1 {
		t.Fatal("an hourly row whose day was never rolled into the daily tier was purged")
	}
	insertDaily(t, ctx, day)
	New(testPool, rollupCfg(), noopLog).Run(ctx)
	if hourlyLeft(t, ctx) != 0 {
		t.Fatal("once its daily row exists the 40-day-old hourly row must be purged")
	}
}

func TestRun_CoverageFollowsTheDailyDays(t *testing.T) {
	_, ctx := requireDB(t)
	cleanRollup(t, ctx)
	base := time.Now().UTC().Truncate(time.Hour)
	for _, age := range []int{36, 399, 401} {
		execRetry(t, ctx, `INSERT INTO sage.query_rollup_coverage (hour, sampled_exec_ms,
			sampled_queryids, epoch_breaks, snapshots, epoch_source)
			VALUES ($1, 1, 4242, 0, 12, 'recorded')`, base.AddDate(0, 0, -age))
		execRetry(t, ctx, `INSERT INTO sage.query_rollup_daily (queryid, day, calls,
			exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, hours, epoch_breaks, plan_hashes)
			VALUES ($1, $2::date, 1, 1, 1, 1, 1, 0, 0, 1, 0, 0)`, rollupQID,
			base.AddDate(0, 0, -age).Format(time.DateOnly))
	}
	New(testPool, rollupCfg(), noopLog).Run(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_coverage
		WHERE sampled_queryids = 4242`); n != 2 {
		t.Fatalf("%d coverage rows left, want 2 (36 and 399 days kept, 401 purged)", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_daily
		WHERE queryid = $1`, rollupQID); n != 2 {
		t.Fatalf("%d daily rows left, want 2 (36 and 399 days kept, 401 purged)", n)
	}
}

// A purge racing the daily roll of the oldest day never leaves the day
// without its source: either the hourly rows survive, or the daily row
// holds all of them.
func TestRun_PurgeRacingTheDailyRollKeepsTheSource(t *testing.T) {
	_, ctx := requireDB(t)
	for round := range 4 {
		cleanRollup(t, ctx)
		day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -40-round)
		for h := range 24 {
			insertRollupHour(t, ctx, day.Add(time.Duration(h)*time.Hour), int64(h+1))
		}
		var wg sync.WaitGroup
		var rollErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, rollErr = querystore.RollupDay(ctx, testPool, day)
		}()
		go func() {
			defer wg.Done()
			for range 3 {
				New(testPool, rollupCfg(), noopLog).Run(ctx)
			}
		}()
		wg.Wait()
		if rollErr != nil {
			t.Fatalf("round %d: roll day: %v", round, rollErr)
		}
		var calls, hours int64
		err := testPool.QueryRow(ctx, `SELECT COALESCE(sum(calls), 0), COALESCE(sum(hours), 0)
			FROM sage.query_rollup_daily WHERE queryid = $1 AND day = $2::date`, rollupQID,
			day.Format(time.DateOnly)).Scan(&calls, &hours)
		if err != nil {
			t.Fatal(err)
		}
		left := hourlyLeft(t, ctx)
		if calls != 300 || hours != 24 {
			t.Fatalf("round %d: daily row calls %d hours %d, want 300 and 24", round, calls,
				hours)
		}
		if left != 0 && left != 24 {
			t.Fatalf("round %d: %d of 24 hourly rows left: purged before the daily row "+
				"committed", round, left)
		}
	}
}

// With equal windows the daily row of a day is purged before the day's
// last hours are: those hours must not then be kept forever for want of a
// daily row. Past the daily window an hour goes without one; with the
// daily tier kept forever (0) it still waits.
func TestRun_UnrolledHourPastTheDailyWindowIsPurged(t *testing.T) {
	_, ctx := requireDB(t)
	cleanRollup(t, ctx)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-31*24*time.Hour - time.Hour)
	insertRollupHour(t, ctx, hour, 1)
	cfg := rollupCfg()
	cfg.Retention.QueryRollupDailyDays = 0
	cfg.Retention.QueryRollupHourlyDays = 30
	New(testPool, cfg, noopLog).Run(ctx)
	if hourlyLeft(t, ctx) != 1 {
		t.Fatal("daily tier kept forever: an unrolled hour must wait for its daily row")
	}
	cfg.Retention.QueryRollupDailyDays = 30
	New(testPool, cfg, noopLog).Run(ctx)
	if hourlyLeft(t, ctx) != 0 {
		t.Fatal("an unrolled hour past the daily window (30 days) must be purged")
	}
	insertRollupHour(t, ctx, time.Now().UTC().Truncate(time.Hour).Add(-29*24*time.Hour), 1)
	New(testPool, cfg, noopLog).Run(ctx)
	if hourlyLeft(t, ctx) != 1 {
		t.Fatal("an hour inside both windows must be kept")
	}
}
