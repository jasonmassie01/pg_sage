package forecaster

import (
	"context"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// forecast-08 on real PostgreSQL: the system read carries each day's
// sample count and span and the new connection counts, Forecast leaves
// today out, and two Forecast calls on one Forecaster share its decoded
// history safely.

const (
	connLegacyDays = 2  // offsets 12 and 11: rows without the new counts
	connNewDays    = 10 // offsets 10..1
)

// seedConnectionDays writes hourly system snapshots: two legacy days, ten
// days with client_backends rising 3 a day from 40 and 3 reserved slots,
// then today's rows at 199 client backends, which must never count.
func seedConnectionDays(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	for off := connLegacyDays + connNewDays; off >= 0; off-- {
		i := connLegacyDays + connNewDays - off
		hours := 24
		if off == 0 {
			hours = 1
		}
		for h := 0; h < hours; h++ {
			row := map[string]any{"total_backends": 100 + 3*i, "active_backends": 5,
				"max_connections": 200, "db_size_bytes": int64(50e9)}
			if off <= connNewDays {
				row["client_backends"] = 40 + 3*i
				row["reserved_connections"] = 3
			}
			if off == 0 {
				row["client_backends"] = 199
			}
			insertSnapshot(t, ctx, pool, dayStart(off).Add(time.Duration(h)*time.Hour),
				"system", row)
		}
	}
}

func TestConnectionLimitDB_SystemReadCarriesCountsAndCoverage(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedConnectionDays(t, ctx, pool)
	aggs, err := QueryDailySystemAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	if len(aggs) != connLegacyDays+connNewDays+1 {
		t.Fatalf("days = %d, want %d (today included in the raw read)", len(aggs),
			connLegacyDays+connNewDays+1)
	}
	if aggs[0].HasClientCounts || aggs[0].MaxClientBackends != 0 {
		t.Errorf("legacy day = %+v, want the client count unknown", aggs[0])
	}
	y := aggs[len(aggs)-2]
	if !y.HasClientCounts || y.MaxClientBackends != 40+3*11 || y.MaxReservedConnections != 3 {
		t.Errorf("yesterday = %+v, want 73 client backends and 3 reserved", y)
	}
	if y.Samples != 24 || !y.FirstAt.Equal(dayStart(1)) ||
		!y.LastAt.Equal(dayStart(1).Add(23*time.Hour)) {
		t.Errorf("yesterday coverage = %d samples %v..%v, want 24 from 00:00 to 23:00",
			y.Samples, y.FirstAt, y.LastAt)
	}
	if today := aggs[len(aggs)-1]; today.Samples != 1 {
		t.Errorf("today = %+v, want 1 sample", today)
	}
}

// Forecast projects client backends of closed days against the usable
// slots: yesterday's 73, not today's 199.
func TestConnectionLimitDB_ForecastLeavesTodayOut(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedConnectionDays(t, ctx, pool)
	cleanupSnapshots(t, pool, ctx, "queries")
	f := New(pool, ForecasterConfig{LookbackDays: 30, ConnectionWarnPct: 80,
		DiskWarnGrowthGBDay: 5, CacheWarnThreshold: 0.5, CollectorInterval: time.Hour},
		func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	conn := findingOf(findings, "forecast_connection_saturation")
	if conn == nil {
		t.Fatalf("no connection finding: %+v", findings)
	}
	d := conn.Detail
	if d["current_in_use"] != 73.0 || d["limit_basis"] != limitBasisClient ||
		d["usable_connections"] != 197.0 {
		t.Fatalf("detail = %v, want 73 client backends of 197 usable", d)
	}
	if d["covered_days"] != connNewDays || d["span_days"] != connNewDays {
		t.Errorf("covered/span = %v/%v, want %d/%d", d["covered_days"], d["span_days"],
			connNewDays, connNewDays)
	}
	if hasCategory(f.LastEvaluatedCategories(), "forecast_query_volume") {
		t.Error("query volume evaluated without query history")
	}
}

// Today stays out on the calendar alone: with no collector interval every
// day with a row is covered, today's single 199-client row included, and
// the projection still ends on yesterday's 73. Its date is yesterday's
// session date plus 28 whole days ((157.6 - 73) / 3 = 28.2), whatever Go's
// Local zone (pgx reads the session's midnights in it).
func TestConnectionLimitDB_TodayOutWithoutCoverageGate(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedConnectionDays(t, ctx, pool)
	cleanupSnapshots(t, pool, ctx, "queries")
	f := New(pool, ForecasterConfig{LookbackDays: 30, ConnectionWarnPct: 80,
		DiskWarnGrowthGBDay: 5, CacheWarnThreshold: 0.5}, func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	conn := findingOf(findings, "forecast_connection_saturation")
	if conn == nil {
		t.Fatalf("no connection finding: %+v", findings)
	}
	if conn.Detail["current_in_use"] != 73.0 {
		t.Errorf("current_in_use = %v, want yesterday's 73", conn.Detail["current_in_use"])
	}
	want := dayStart(1).AddDate(0, 0, 28).Format(dateLayout)
	if conn.Detail["projected_date"] != want {
		t.Errorf("projected_date = %v, want %s", conn.Detail["projected_date"], want)
	}
}

// seedQueryWeeks writes hourly query snapshots for 15 closed days and
// today's midnight: one query at 100 calls an hour, 160 in the last week.
func seedQueryWeeks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanupSnapshots(t, pool, ctx, "queries")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "queries") })
	calls := int64(1000)
	for off := 15; off >= 0; off-- {
		for h := 0; h < 24 && (off > 0 || h == 0); h++ {
			at := dayStart(off).Add(time.Duration(h) * time.Hour)
			// The calls since the previous sample belong to that hour's day.
			rate := int64(100)
			if !at.Add(-time.Hour).Before(dayStart(7)) {
				rate = 160
			}
			calls += rate
			insertSnapshot(t, ctx, pool, at, "queries",
				[]map[string]any{{"queryid": 8008, "calls": calls}})
		}
	}
}

// Concurrency: two Forecast calls on one Forecaster share dayHistory.
// Both succeed with the same findings, and the query volume is the 60%
// of the two weeks ending yesterday.
func TestConnectionLimitDB_ConcurrentForecastsShareHistory(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedConnectionDays(t, ctx, pool)
	seedQueryWeeks(t, ctx, pool)
	f := New(pool, ForecasterConfig{LookbackDays: 30, ConnectionWarnPct: 80,
		DiskWarnGrowthGBDay: 5, CacheWarnThreshold: 0.5, CollectorInterval: time.Hour},
		func(string, string, ...any) {})
	var wg sync.WaitGroup
	results := make([][]analyzer.Finding, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = f.Forecast(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Forecast %d: %v", i, err)
		}
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("concurrent runs differ:\n%+v\n%+v", results[0], results[1])
	}
	q := findingOf(results[0], "forecast_query_volume")
	if q == nil {
		t.Fatalf("no query volume finding: %+v", results[0])
	}
	if g := q.Detail["wow_growth_pct"].(float64); math.Abs(g-60) > 1e-6 {
		t.Errorf("wow_growth_pct = %v, want 60", g)
	}
	if !hasCategory(f.LastEvaluatedCategories(), "forecast_query_volume") {
		t.Errorf("query volume not evaluated: %v", f.LastEvaluatedCategories())
	}
}

// Error propagation: a failed system read (a non-numeric size) claims no
// system category, and query volume, read separately, stays evaluated.
func TestConnectionLimitDB_QueryVolumeSurvivesSystemFailure(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedQueryWeeks(t, ctx, pool)
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	insertSnapshot(t, ctx, pool, dayStart(1).Add(time.Hour), "system",
		map[string]any{"db_size_bytes": "abc", "max_connections": 100})
	var warned bool
	f := New(pool, ForecasterConfig{LookbackDays: 30, CollectorInterval: time.Hour},
		func(level, format string, _ ...any) { warned = warned || level == "WARN" })
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	if !warned {
		t.Error("the failed system read was not logged")
	}
	got := f.LastEvaluatedCategories()
	for _, c := range systemForecastCategories {
		if hasCategory(got, c) {
			t.Errorf("%s claimed after a failed system read: %v", c, got)
		}
	}
	if !hasCategory(got, "forecast_query_volume") ||
		findingOf(findings, "forecast_query_volume") == nil {
		t.Fatalf("query volume = %v / %+v, want evaluated with its finding", got, findings)
	}
}

func findingOf(findings []analyzer.Finding, category string) *analyzer.Finding {
	for i := range findings {
		if findings[i].Category == category {
			return &findings[i]
		}
	}
	return nil
}
