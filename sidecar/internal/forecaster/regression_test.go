package forecaster

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// Regression tests for forecaster bugs recorded in reviews/2026-09-26.

// C01/G2-B04: cache hit ratio is a fraction (0-1). Healthy fractional data
// must not trigger a warning, and titles must render percent correctly.
func TestRegression_CachePressureFractionUnits(t *testing.T) {
	cfg := ForecasterConfig{CacheWarnThreshold: 0.95}
	healthy := makeSysAggs(10, func(_ int) DaySystemAgg {
		return DaySystemAgg{AvgCacheHitRatio: 0.99}
	})
	if got := forecastCachePressure(healthy, cfg); len(got) != 0 {
		t.Fatalf("healthy 99%% fraction flagged: %+v", got)
	}
	low := makeSysAggs(10, func(_ int) DaySystemAgg {
		return DaySystemAgg{AvgCacheHitRatio: 0.80}
	})
	got := forecastCachePressure(low, cfg)
	if len(got) == 0 {
		t.Fatal("80% fraction not flagged")
	}
	if !strings.Contains(got[0].Title, "80.0%") {
		t.Fatalf("title = %q, want 80.0%%", got[0].Title)
	}
}

// C01: the daily average must normalise mixed legacy percent rows and
// fraction rows, and ignore the no-data sentinel (-1).
func TestRegression_SystemAggsNormaliseCacheRatio(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "system")
	t.Cleanup(func() { cleanupSnapshots(t, pool, ctx, "system") })
	now := time.Now()
	for _, ratio := range []float64{99.0, 0.99, -1} {
		raw, err := json.Marshal(map[string]any{
			"cache_hit_ratio": ratio, "max_connections": 100,
			"db_size_bytes": 1000, "active_backends": 1,
			"total_backends": 2, "total_checkpoints": 5,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO sage.snapshots (collected_at, category, data)
			 VALUES ($1, 'system', $2::jsonb)`, now, string(raw)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	aggs, err := QueryDailySystemAggs(ctx, pool, 7)
	if err != nil {
		t.Fatalf("QueryDailySystemAggs: %v", err)
	}
	if len(aggs) != 1 || math.Abs(aggs[0].AvgCacheHitRatio-0.99) > 1e-9 {
		t.Fatalf("aggs = %+v, want one day with ratio 0.99", aggs)
	}
}

// G2-B18: idle connections consume slots; saturation must use total
// backends, not active ones.
func TestRegression_ConnectionSaturationUsesTotalBackends(t *testing.T) {
	aggs := makeSysAggs(30, func(i int) DaySystemAgg {
		return DaySystemAgg{
			MaxActiveBackends: 20,
			MaxTotalBackends:  float64(600 + i*10),
			MaxConnections:    1000,
		}
	})
	got := forecastConnectionSaturation(aggs, ForecasterConfig{ConnectionWarnPct: 80})
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1 (total backends trending up)", len(got))
	}
}

// C10: constant traffic of 100 calls/day must yield ~100 calls/day, not
// the growing lifetime counter; a counter reset must not go negative.
func TestRegression_QueryAggsUseResetAwareDeltas(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cleanupSnapshots(t, pool, ctx, "queries")
	t.Cleanup(func() { cleanupSnapshots(t, pool, ctx, "queries") })
	seedQuerySnapshots(t, pool, ctx, 14, func(i int) []queryElem {
		calls := int64(100 * (i + 1))
		if i >= 10 {
			calls = int64(100 * (i - 9)) // pg_stat_statements_reset at day 10
		}
		return []queryElem{{QueryID: 9101, Calls: calls}}
	})
	aggs, err := QueryDailyQueryAggs(ctx, pool, 30)
	if err != nil {
		t.Fatalf("QueryDailyQueryAggs: %v", err)
	}
	if len(aggs) != 14 {
		t.Fatalf("days = %d, want 14", len(aggs))
	}
	for i, a := range aggs[1:] {
		if a.TotalCalls != 100 {
			t.Fatalf("day %d calls = %v, want 100", i+1, a.TotalCalls)
		}
	}
	if got := forecastQueryVolume(aggs, ForecasterConfig{}); len(got) != 0 {
		t.Fatalf("constant traffic flagged as growth: %+v", got)
	}
}
