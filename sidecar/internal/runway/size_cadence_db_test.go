package runway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Size sampling cadence against real PostgreSQL: between measurements
// the monitor writes WAL samples every tick and no database size sample,
// so the trend over the samples counts each measurement once.

func sampleCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.runway_samples
		WHERE kind = $1 AND subject = $2`, kind, probes.SubjectCluster).Scan(&n); err != nil {
		t.Fatalf("count %s samples: %v", kind, err)
	}
	return n
}

func TestSizeCadence_SamplesOnlyMeasuredSizes(t *testing.T) {
	_, ctx := livePool(t)
	pool := fleetDatabase(t, ctx, "runway_size_cadence")
	share, clock := newTestShare()
	runner := countingRunner{base: probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(probes.MaxSidecarConcurrency)), sizes: &atomic.Int64{}}
	opts := testOptions()
	opts.Interval, opts.Retention, opts.Sizes = time.Minute, 48*time.Hour, share
	opts.SizeInterval = 10 * time.Minute
	m, err := NewMonitor(pool, runner, nil, opts, nil)
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Sample(ctx); err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		clock.advance(time.Minute)
	}
	wal, dbs := sampleCount(t, ctx, pool, probes.RunwayWALPosition),
		sampleCount(t, ctx, pool, probes.RunwayDatabaseBytes)
	if wal != 3 || dbs != 1 || runner.sizes.Load() != 1 {
		t.Fatalf("3 ticks inside the size interval: %d wal, %d size samples, %d "+
			"measurements; want 3, 1, 1", wal, dbs, runner.sizes.Load())
	}
	clock.advance(10 * time.Minute)
	if _, err := m.Sample(ctx); err != nil {
		t.Fatalf("sample after the interval: %v", err)
	}
	if dbs := sampleCount(t, ctx, pool, probes.RunwayDatabaseBytes); dbs != 2 ||
		runner.sizes.Load() != 2 {
		t.Fatalf("after the size interval: %d size samples, %d measurements; want 2, 2",
			dbs, runner.sizes.Load())
	}
	trends, err := probes.RunwayTrends(runner.Run(ctx, probes.RunwayTrendsProbe,
		probes.Args{Window: time.Hour}))
	if err != nil {
		t.Fatalf("runway_trends: %v", err)
	}
	tr, ok := probes.FindTrend(trends, probes.RunwayDatabaseBytes, probes.SubjectCluster)
	if !ok || tr.Samples != 2 {
		t.Fatalf("database_bytes trend %+v (found %v), want 2 samples", tr, ok)
	}
}
