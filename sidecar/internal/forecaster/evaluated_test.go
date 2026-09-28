package forecaster

import (
	"context"
	"testing"
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
	for _, c := range []string{"forecast_disk_growth", "forecast_query_volume",
		"forecast_sequence_exhaustion", "forecast_cache_pressure"} {
		if !got[c] {
			t.Fatalf("missing %s in %v", c, got)
		}
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
