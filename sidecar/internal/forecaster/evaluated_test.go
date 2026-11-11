package forecaster

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// G2-B02: the forecaster reports only categories whose data source read
// succeeded; with a cancelled context every source fails and nothing is claimed.
func TestLastEvaluatedCategories_Success(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	f := New(pool, ForecasterConfig{LookbackDays: 7}, func(string, string, ...any) {})
	if _, err := f.Forecast(ctx); err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	got := map[string]bool{}
	for _, c := range f.LastEvaluatedCategories() {
		got[c] = true
	}
	// forecast-04: forecast_cache_pressure needs 7 days of interval data,
	// which an empty history lacks; rules_cache_test.go covers it.
	if !got["forecast_disk_growth"] {
		t.Fatalf("missing forecast_disk_growth in %v", got)
	}
	// forecast-08: query volume needs two complete weeks of closed days,
	// which an empty history lacks; connection_limit_db_test.go covers it.
	if got["forecast_query_volume"] {
		t.Fatalf("forecast_query_volume evaluated without history: %v", got)
	}
	// forecast-05: the runway forecasts sequences; the forecaster's rule is
	// retired and must never again claim (and so resolve) its category.
	if got["forecast_sequence_exhaustion"] {
		t.Fatalf("the retired forecast_sequence_exhaustion is evaluated: %v", got)
	}
}

func TestLastEvaluatedCategories_FailedSources(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	f := New(pool, ForecasterConfig{LookbackDays: 7}, func(string, string, ...any) {})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Forecast(cctx); err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	if got := f.LastEvaluatedCategories(); len(got) != 0 {
		t.Fatalf("categories claimed after failed reads: %v", got)
	}
}

// forecast-07: a malformed size is a broken read, not an unknown size. It
// still fails the system read, and none of the four system categories is
// claimed; the other sources stay evaluated.
func TestLastEvaluatedCategories_MalformedSizeFailsSystemRead(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "system") })
	insertSnapshot(t, ctx, pool, time.Now(), "system", map[string]any{
		"db_size_bytes": "abc", "active_backends": 1, "max_connections": 100})
	var warned bool
	f := New(pool, ForecasterConfig{LookbackDays: 7},
		func(level, format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if level == "WARN" && strings.Contains(msg, "system aggs") {
				warned = true
			}
		})
	if _, err := QueryDailySystemAggs(ctx, pool, 7); err == nil {
		t.Fatal("a non-numeric db_size_bytes must still fail the read")
	}
	if _, err := f.Forecast(ctx); err != nil {
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
	// forecast-08: without two complete weeks of query history the query
	// category is not evaluated either; that the query read still runs
	// after a failed system read is TestConnectionLimitDB_QueryVolumeSurvivesSystemFailure.
	if hasCategory(got, "forecast_query_volume") {
		t.Errorf("query volume evaluated without query history: %v", got)
	}
}
