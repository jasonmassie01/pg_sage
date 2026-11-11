package forecaster

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests: aggregation only reads immutable seeded snapshots.
func TestAuditQueryAggsNullableSnapshots(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "queries")
	// Two samples: calls are cumulative, so the day total is the delta
	// 22 - 11 = 11 (C10).
	for _, raw := range []string{"null", `[{"queryid":7,"calls":11}]`, "[]",
		`[{"queryid":7,"calls":22}]`} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.snapshots
   (collected_at, category, data) VALUES (now(), 'queries', $1::jsonb)`, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := QueryDailyQueryAggs(ctx, pool, 1)
	if err != nil {
		t.Fatalf("nullable snapshots must preserve valid query counts: %v", err)
	}
	if len(got) != 1 || got[0].TotalCalls != 11 {
		t.Fatalf("got %#v; want one day with 11 calls", got)
	}
	got, err = QueryDailyQueryAggs(ctx, pool, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("zero lookback got %#v, %v", got, err)
	}
}

// forecast-07: every row of one day wrote db_size_bytes as JSON null (the
// size was never measured). The day must come back as unknown, not fail
// the read and with it all four system forecast categories.
const nullDay = 3

func seedNullSizeDay(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	for i := 0; i < 8; i++ {
		size := 100e9 + float64(i)*6e9
		for h := 11; h <= 12; h++ {
			row := map[string]any{"db_size_bytes": int64(size), "active_backends": 5,
				"total_backends": 10 + i, "max_connections": 100,
				"cache_hit_ratio": 0.99, "total_checkpoints": 1000 + i*24}
			switch {
			case i == nullDay:
				row["db_size_bytes"] = nil
			case i == 5 && h == 11:
				row["db_size_bytes"] = 0 // legacy rows wrote 0 for "unknown"
			case i == 6 && h == 11:
				row["max_connections"] = nil
			}
			// The newest day is yesterday: forecast-08 leaves today's
			// partial day out of the rules.
			at := dayStart(8 - i).Add(time.Duration(h) * time.Hour)
			insertSnapshot(t, ctx, pool, at, "system", row)
		}
	}
}

func TestAuditSystemAggsNullableSnapshots(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedNullSizeDay(t, ctx, pool)
	got, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("an all-null db_size_bytes day must not fail the read: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("got %d days, want 8: %+v", len(got), got)
	}
	for i, a := range got {
		if a.DBSizeUnknown != (i == nullDay) {
			t.Errorf("day %d: DBSizeUnknown = %v", i, a.DBSizeUnknown)
		}
		if a.MaxConnectionsUnknown || a.BackendsUnknown {
			t.Errorf("day %d: known inputs marked unknown: %+v", i, a)
		}
		// forecast-03: the seed is legacy rows (total_checkpoints only),
		// which carry no requested count.
		if !a.CheckpointsUnknown {
			t.Errorf("day %d: legacy rows read as split checkpoint counters: %+v", i, a)
		}
		if a.MaxConnections != 100 {
			t.Errorf("day %d: MaxConnections = %v, want 100", i, a.MaxConnections)
		}
	}
	if a := got[nullDay]; a.MaxDBSizeBytes != 0 || a.AvgDBSizeBytes != 0 {
		t.Errorf("unknown day carries sizes: %+v", a)
	}
	if want := 100e9 + 5*6e9; got[5].AvgDBSizeBytes != want {
		t.Errorf("day 5 avg size = %v, want %v: a 0 size was averaged in",
			got[5].AvgDBSizeBytes, want)
	}
}

func TestAuditForecastNullSizeDayKeepsSystemCategories(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedNullSizeDay(t, ctx, pool)
	var warns []string
	f := New(pool, ForecasterConfig{LookbackDays: 30, DiskWarnGrowthGBDay: 5,
		ConnectionWarnPct: 80, CacheWarnThreshold: 0.5},
		func(level, format string, args ...any) {
			if level == "WARN" {
				warns = append(warns, fmt.Sprintf(format, args...))
			}
		})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	for _, w := range warns {
		if strings.Contains(w, "system aggs") {
			t.Fatalf("null-size day logged a system aggs WARN: %s", w)
		}
	}
	cats := f.LastEvaluatedCategories()
	for _, c := range systemForecastCategories {
		// forecast-03: legacy rows leave the checkpoint rule unevaluated;
		// forecast-04: and the cache rule (no interval counters).
		want := c != "forecast_checkpoint_pressure" && c != "forecast_cache_pressure"
		if hasCategory(cats, c) != want {
			t.Errorf("%s evaluated = %v, want %v: %v", c, !want, want, cats)
		}
	}
	var disk map[string]any
	for _, fd := range findings {
		if fd.Category == "forecast_disk_growth" {
			disk = fd.Detail
		}
	}
	if disk == nil {
		t.Fatalf("no disk finding from 7 known days growing 6 GB/day: %+v", findings)
	}
	if rate := disk["growth_rate_gb_day"].(float64); math.Abs(rate-6) > 0.05 {
		t.Errorf("growth_rate_gb_day = %v, want about 6", rate)
	}
	if cur := disk["current_size_gb"].(float64); math.Abs(cur-142) > 0.01 {
		t.Errorf("current_size_gb = %v, want 142", cur)
	}
}
