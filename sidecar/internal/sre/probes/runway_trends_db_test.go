package probes

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
)

// forecast-01: runway_trends v3 regresses each series twice in one scan:
// rate_per_s by its counter (XIDs consumed per second) and
// value_rate_per_s by its value (the net age slope, which freezing
// bends). A level series without a counter has both equal.
//
// No concurrency test: one read-only statement per call.
func TestRunwayTrends_V3ReturnsConsumptionAndNetAgeSlope(t *testing.T) {
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	subject := fmt.Sprintf("v3_trend_%d", time.Now().UnixNano())
	// Counter up 2,000/s; the age up 500/s (frozen behind it); a level
	// series rising 3/s.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'xid', $1, 'e', now() - make_interval(secs => 40 - i * 10),
		       2.1e8 + i * 5000, 3e9 + i * 20000, 2107483647
		FROM generate_series(0, 4) i
		UNION ALL
		SELECT 'database_bytes', $1, 'e', now() - make_interval(secs => 40 - i * 10),
		       100 + i * 30, NULL, NULL
		FROM generate_series(0, 4) i`, subject); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.runway_samples WHERE subject = $1", subject)
	})
	res := catalogRun(t, ctx, pool, RunwayTrendsProbe, Args{Window: time.Minute})
	if res.Version != "v3" {
		t.Fatalf("runway_trends version %q, want v3", res.Version)
	}
	ts, err := RunwayTrends(res)
	if err != nil {
		t.Fatalf("trends: %v", err)
	}
	x, ok := FindTrend(ts, RunwayXID, subject)
	if !ok || x.Samples != 5 || math.Abs(x.RatePerS-2000) > 1e-6 ||
		math.Abs(x.ValueRatePerS-500) > 1e-6 || x.LastValue != 2.1e8+20000 {
		t.Fatalf("xid series = %+v (%v), want consumption 2000/s, net slope 500/s", x, ok)
	}
	db, ok := FindTrend(ts, RunwayDatabaseBytes, subject)
	if !ok || math.Abs(db.RatePerS-3) > 1e-6 || math.Abs(db.ValueRatePerS-3) > 1e-6 {
		t.Fatalf("level series = %+v (%v), want both slopes 3/s", db, ok)
	}
}
