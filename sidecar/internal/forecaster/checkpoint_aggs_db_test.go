package forecaster

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// forecast-03 on real PostgreSQL: the daily aggregation carries the split
// checkpoint counters of closed days only, with the time of each day's
// last split sample, and today's partial day never moves the result.

// No concurrent access tests: the aggregation only reads seeded rows,
// and the cross-package lock serialises the tests that write them.

func ckptRow(timed, requested int64) map[string]any {
	return map[string]any{"db_size_bytes": int64(1e9), "active_backends": 1,
		"total_backends": 5, "max_connections": 100, "cache_hit_ratio": 0.99,
		"total_checkpoints": timed + requested, "checkpoints_timed": timed,
		"checkpoints_requested": requested}
}

// seedCheckpointWeek writes days closed days of hourly system snapshots
// ending yesterday: timedPerHour and requestedPerHour checkpoints an hour.
func seedCheckpointWeek(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	days int, timedPerHour, requestedPerHour int64) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	var timed, requested int64 = 90000, 400
	for d := days; d >= 1; d-- {
		for h := 0; h < 24; h++ {
			insertSnapshot(t, ctx, pool, dayStart(d).Add(time.Duration(h)*time.Hour+
				30*time.Minute), "system", ckptRow(timed, requested))
			timed += timedPerHour
			requested += requestedPerHour
		}
	}
}

func TestCheckpointAggs_ClosedDaysCarryTheSplit(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	for d := 3; d >= 1; d-- {
		for i, h := range []int{1, 7, 13, 19} {
			n := int64(10*(3-d) + i)
			insertSnapshot(t, ctx, pool, dayStart(d).Add(time.Duration(h)*time.Hour),
				"system", ckptRow(1000+n*12, 50+n))
		}
	}
	legacy := map[string]any{"max_connections": 100, "total_checkpoints": 99999}
	insertSnapshot(t, ctx, pool, dayStart(1).Add(22*time.Hour), "system", legacy)
	insertSnapshot(t, ctx, pool, time.Now().Add(-time.Minute), "system", ckptRow(5000, 900))

	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	if len(aggs) != 4 {
		t.Fatalf("got %d days, want 3 closed days and today: %+v", len(aggs), aggs)
	}
	for i, a := range aggs[:3] {
		n := float64(10*i + 3)
		if a.CheckpointsUnknown || a.CheckpointsRequested != 50+n ||
			a.CheckpointsTimed != 1000+n*12 {
			t.Errorf("day %d: split = %+v, want the day maxima %v/%v", i, a, 1000+n*12, 50+n)
		}
		want := dayStart(3 - i).Add(19 * time.Hour)
		if !a.CheckpointsLastAt.Equal(want) {
			t.Errorf("day %d: last split sample %v, want %v (a legacy row is not one)",
				i, a.CheckpointsLastAt, want)
		}
	}
	if today := aggs[3]; !today.CheckpointsUnknown || today.CheckpointsRequested != 0 ||
		!today.CheckpointsLastAt.IsZero() {
		t.Errorf("today's partial day carries checkpoint counters: %+v", today)
	}
}

func TestCheckpointAggs_LegacyRowsAreUnknown(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	insertSnapshot(t, ctx, pool, dayStart(2).Add(time.Hour), "system",
		map[string]any{"max_connections": 100, "total_checkpoints": 300})
	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil || len(aggs) != 1 {
		t.Fatalf("aggs = %+v (%v)", aggs, err)
	}
	if !aggs[0].CheckpointsUnknown || !aggs[0].CheckpointsLastAt.IsZero() {
		t.Fatalf("a row without the split reads as known: %+v", aggs[0])
	}
}

func checkpointRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (
	*analyzer.Finding, []string) {
	t.Helper()
	f := New(pool, ForecasterConfig{LookbackDays: 30}, func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	for i := range findings {
		if findings[i].Category == "forecast_checkpoint_pressure" {
			return &findings[i], f.LastEvaluatedCategories()
		}
	}
	return nil, f.LastEvaluatedCategories()
}

func TestCheckpointForecast_IdleWeekGivesNoFinding(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	// checkpoint_timeout=60s on an idle server: 60 timed an hour, every
	// one skipped, and no requested checkpoint.
	seedCheckpointWeek(t, ctx, pool, 8, 60, 0)
	got, cats := checkpointRun(t, ctx, pool)
	if got != nil {
		t.Fatalf("an idle week warned: %+v", got)
	}
	if !hasCategory(cats, "forecast_checkpoint_pressure") {
		t.Fatalf("8 closed days of split counters must evaluate the category: %v", cats)
	}
}

func TestCheckpointForecast_TodayDoesNotMoveTheResult(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCheckpointWeek(t, ctx, pool, 9, 12, 3)
	base, _ := checkpointRun(t, ctx, pool)
	if base == nil {
		t.Fatal("3 requested checkpoints an hour must warn")
	}
	if got := base.Detail["ewma_requested_per_hour"].(float64); got < 2.99 || got > 3.01 {
		t.Fatalf("ewma_requested_per_hour = %v, want about 3", got)
	}
	// Evaluated at 00:30: today holds one sample. At 23:30 it holds a
	// burst of 5,000 requested checkpoints. Neither is a closed day.
	insertSnapshot(t, ctx, pool, dayStart(0).Add(30*time.Minute), "system",
		ckptRow(1e6, 1e6))
	early, _ := checkpointRun(t, ctx, pool)
	insertSnapshot(t, ctx, pool, dayStart(0).Add(23*time.Hour+30*time.Minute), "system",
		ckptRow(1e6+300, 1e6+5000))
	late, _ := checkpointRun(t, ctx, pool)
	for name, f := range map[string]*analyzer.Finding{"00:30": early, "23:30": late} {
		if f == nil || f.Title != base.Title ||
			f.Detail["ewma_requested_per_hour"] != base.Detail["ewma_requested_per_hour"] ||
			len(hourlyRates(t, f.Detail)) != len(hourlyRates(t, base.Detail)) {
			t.Errorf("evaluated at %s: %+v, want the closed-day result %+v", name, f, base)
		}
	}
}

func TestCheckpointForecast_MidDayResetIsSkipped(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCheckpointWeek(t, ctx, pool, 10, 12, 3)
	steady, _ := checkpointRun(t, ctx, pool)
	if steady == nil {
		t.Fatal("3 requested checkpoints an hour must warn")
	}
	// pg_stat_reset_shared at noon five days ago: from then on the
	// counters restart from 0 and keep growing 12 timed and 3 requested
	// an hour. The day maximum is the pre-reset value; read as the day's
	// counter it would give the reset day a rate of 1.5/hr.
	cleanupSnapshots(t, pool, ctx, "system")
	var timed, requested int64 = 90000, 400
	for d := 10; d >= 1; d-- {
		for h := 0; h < 24; h++ {
			if d == 5 && h == 12 {
				timed, requested = 0, 0
			}
			insertSnapshot(t, ctx, pool, dayStart(d).Add(time.Duration(h)*time.Hour+
				30*time.Minute), "system", ckptRow(timed, requested))
			timed += 12
			requested += 3
		}
	}
	got, _ := checkpointRun(t, ctx, pool)
	if got == nil {
		t.Fatal("the reset removed the finding")
	}
	rates := hourlyRates(t, got.Detail)
	assertRates(t, rates, len(hourlyRates(t, steady.Detail))-1, 3)
	if got.Detail["ewma_requested_per_hour"] != steady.Detail["ewma_requested_per_hour"] {
		t.Fatalf("ewma %v with a mid-day reset, %v without: the reset day was read",
			got.Detail["ewma_requested_per_hour"], steady.Detail["ewma_requested_per_hour"])
	}
}
