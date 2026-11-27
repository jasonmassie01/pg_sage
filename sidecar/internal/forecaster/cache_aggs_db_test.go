package forecaster

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// forecast-04 on real PostgreSQL: the daily aggregation sums the interval
// block counts collection-06 writes per system snapshot, over closed days
// only. The lifetime cache_hit_ratio is no longer read.

// No concurrent access tests: the aggregation only reads seeded rows,
// and the cross-package lock serialises the tests that write them.

const cacheSeedDays = 8

// cacheRow is a system snapshot whose lifetime ratio is a healthy 99.5%
// while its interval says what the last minute did; a nil interval ratio
// with nil blocks is a reset (collection-06), no interval key a legacy row.
func cacheRow(accessed *int64, ratio *float64) map[string]any {
	return map[string]any{"db_size_bytes": int64(1e9), "active_backends": 1,
		"total_backends": 5, "max_connections": 100, "cache_hit_ratio": 0.995,
		"interval": map[string]any{"seconds": 3600, "blks_accessed": accessed,
			"cache_hit_ratio": ratio}}
}

func i64(v int64) *int64        { return &v }
func f64p(v float64) *float64   { return &v }
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// seedCacheDays writes cacheSeedDays closed days of hourly snapshots,
// 10,000 blocks an hour at 99%, with the newest closed day ("day N") at
// 80%; day 3 has one reset snapshot and day 5 is legacy rows only.
// Today's rows read 10% and must never count.
func seedCacheDays(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	for d := cacheSeedDays; d >= 1; d-- {
		ratio := 0.99
		if d == 1 {
			ratio = 0.80
		}
		for h := 0; h < 24; h++ {
			at := dayStart(d).Add(time.Duration(h)*time.Hour + 30*time.Minute)
			row := cacheRow(i64(10_000), f64p(ratio))
			switch {
			case d == 5:
				delete(row, "interval")
			case d == 3 && h == 12:
				row = cacheRow(nil, nil)
			}
			insertSnapshot(t, ctx, pool, at, "system", row)
		}
	}
	insertSnapshot(t, ctx, pool, time.Now().Add(-time.Minute), "system",
		cacheRow(i64(10_000_000), f64p(0.10)))
}

func TestCacheAggs_IntervalRatioOfClosedDays(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCacheDays(t, ctx, pool)
	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	byDay := map[string]DaySystemAgg{}
	for _, a := range aggs {
		byDay[dayKey(a.Day.UTC())] = a
	}
	for d := cacheSeedDays; d >= 1; d-- {
		a, ok := byDay[dayKey(dayStart(d))]
		if !ok {
			t.Fatalf("day %d missing from %+v", d, aggs)
		}
		wantRatio, wantBlocks := 0.99, 240_000.0
		switch d {
		case 1:
			wantRatio = 0.80
		case 3:
			wantBlocks = 230_000 // the reset snapshot adds nothing
		case 5:
			if !a.CacheUnknown {
				t.Errorf("legacy day 5 is known: %+v", a)
			}
			continue
		}
		if a.CacheUnknown || math.Abs(a.CacheHitRatio-wantRatio) > 1e-9 ||
			a.CacheBlocksAccessed != wantBlocks {
			t.Errorf("day %d: ratio %v blocks %v unknown %v, want %v and %v",
				d, a.CacheHitRatio, a.CacheBlocksAccessed, a.CacheUnknown,
				wantRatio, wantBlocks)
		}
	}
	if today, ok := byDay[dayKey(dayStart(0))]; !ok || !today.CacheUnknown ||
		today.CacheHitRatio != 0 {
		t.Errorf("today's partial day must be unknown: %+v (found %v)", today, ok)
	}
}

// The lifetime ratio of every row is 99.5%, which never warns at 95%; the
// interval ratio of day N is 80%, so the forecast run on day N+1 warns.
func TestCacheForecast_IntervalDropWarnsWhereLifetimeWouldNot(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCacheDays(t, ctx, pool)
	f := New(pool, ForecasterConfig{LookbackDays: 30, CacheWarnThreshold: 0.95},
		func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	if !hasCategory(f.LastEvaluatedCategories(), "forecast_cache_pressure") {
		t.Fatalf("seven usable days do not evaluate the cache category: %v",
			f.LastEvaluatedCategories())
	}
	var low map[string]any
	for _, fd := range findings {
		if fd.Category == "forecast_cache_pressure" &&
			fd.Detail["forecast_type"] == "cache_pressure" {
			low = fd.Detail
			if !strings.Contains(fd.Title, "80.0%") {
				t.Errorf("title = %q, want day N's 80.0%%", fd.Title)
			}
		}
	}
	if low == nil {
		t.Fatalf("no low finding for day N at 80%%: %+v", findings)
	}
	if day, _ := low["day"].(time.Time); !day.Equal(dayStart(1)) ||
		low["days_evaluated"].(int) != 7 {
		t.Errorf("detail = %+v, want day %s over 7 usable days", low, dayStart(1))
	}
	if sb, _ := low["shared_buffers"].(string); sb == "" {
		t.Errorf("shared_buffers missing from the evidence: %+v", low)
	}
}

// A day under forecaster.cache_min_blocks_per_day is unknown: with the
// gate above every seeded day's 240,000 blocks, nothing is evaluated.
func TestCacheForecast_MinBlocksGate(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCacheDays(t, ctx, pool)
	f := New(pool, ForecasterConfig{LookbackDays: 30, CacheWarnThreshold: 0.95,
		CacheMinBlocksPerDay: 240_001}, func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	for _, fd := range findings {
		if fd.Category == "forecast_cache_pressure" {
			t.Fatalf("days under the gate gave a finding: %+v", fd)
		}
	}
	if hasCategory(f.LastEvaluatedCategories(), "forecast_cache_pressure") {
		t.Fatal("days under the gate evaluate the cache category")
	}
}

// tablesDoc is a 'tables' snapshot of heap_blks_read per table.
func tablesDoc(reads map[string]int64) []map[string]any {
	out := make([]map[string]any, 0, len(reads))
	for name, n := range reads {
		out = append(out, map[string]any{"schemaname": "public", "relname": name,
			"heap_blks_read": n})
	}
	return out
}

func TestCacheTopTables_HeapReadDeltasOfTheDay(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "tables")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "tables") })
	day := dayStart(1)
	insertSnapshot(t, ctx, pool, day.Add(time.Hour), "tables", tablesDoc(map[string]int64{
		"a": 100, "b": 100, "c": 100, "d": 100, "e": 100, "f": 100, "reset": 9000}))
	insertSnapshot(t, ctx, pool, day.Add(12*time.Hour), "tables", tablesDoc(map[string]int64{
		"a": 1}))
	insertSnapshot(t, ctx, pool, day.Add(23*time.Hour), "tables", tablesDoc(map[string]int64{
		"a": 600, "b": 5100, "c": 300, "d": 200, "e": 150, "f": 101, "reset": 10,
		"new": 99999}))
	// The previous and the next day must not be read.
	insertSnapshot(t, ctx, pool, dayStart(2).Add(time.Hour), "tables",
		tablesDoc(map[string]int64{"a": 0}))
	insertSnapshot(t, ctx, pool, dayStart(0).Add(time.Minute), "tables",
		tablesDoc(map[string]int64{"a": 1e9}))

	got, err := queryCacheTopTables(ctx, pool, day, 5)
	if err != nil {
		t.Fatalf("queryCacheTopTables: %v", err)
	}
	want := []tableHeapReads{{"public", "b", 5000}, {"public", "a", 500},
		{"public", "c", 200}, {"public", "d", 100}, {"public", "e", 50}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rank %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
	none, err := queryCacheTopTables(ctx, pool, dayStart(5), 5)
	if err != nil || len(none) != 0 {
		t.Fatalf("a day without table snapshots = %+v, %v; want none", none, err)
	}
}

func TestCacheTopTables_ErrorNamesTheRead(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := queryCacheTopTables(cctx, pool, dayStart(1), 5); err == nil ||
		!strings.Contains(err.Error(), "cache top tables") {
		t.Fatalf("cancelled read error = %v, want one naming the cache top tables", err)
	}
}
