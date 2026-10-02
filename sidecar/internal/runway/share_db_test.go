package runway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Fleet WAL-runway dedupe against real PostgreSQL: two fleet runtimes on
// two databases of one cluster tick together; the databases' total size
// is measured once and both sample the same cluster-level value.

// countingRunner counts the database-size probes of every runtime that
// shares sizes.
type countingRunner struct {
	base  ProbeRunner
	sizes *atomic.Int64
}

func (c countingRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	if id == probes.ClusterDatabaseSizeProbe {
		c.sizes.Add(1)
	}
	return c.base.Run(ctx, id, a)
}

// RunBackground counts like Run: the sequence sampler uses the background
// budget, and size probes must be counted on either path.
func (c countingRunner) RunBackground(
	ctx context.Context, id probes.ID, a probes.Args,
) probes.Result {
	if id == probes.ClusterDatabaseSizeProbe {
		c.sizes.Add(1)
	}
	return c.base.RunBackground(ctx, id, a)
}

func fleetDatabase(t *testing.T, ctx context.Context, label string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return pool
}

func sampledDatabaseBytes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) float64 {
	t.Helper()
	var v float64
	if err := pool.QueryRow(ctx, `SELECT value FROM sage.runway_samples
		WHERE kind = $1 AND subject = $2 ORDER BY sampled_at DESC LIMIT 1`,
		probes.RunwayDatabaseBytes, probes.SubjectCluster).Scan(&v); err != nil {
		t.Fatalf("read database_bytes sample: %v", err)
	}
	return v
}

func fleetMonitors(t *testing.T, ctx context.Context, share *SizeShare) ([]*Monitor,
	[]*pgxpool.Pool, *atomic.Int64) {
	t.Helper()
	sizes := &atomic.Int64{}
	var monitors []*Monitor
	var pools []*pgxpool.Pool
	limiter := probes.NewLimiter(probes.MaxSidecarConcurrency)
	for _, label := range []string{"runway_fleet_a", "runway_fleet_b"} {
		pool := fleetDatabase(t, ctx, label)
		runner := countingRunner{base: probes.NewRunner(pool, probes.Catalog(), limiter),
			sizes: sizes}
		opts := testOptions()
		opts.Interval, opts.Retention, opts.Sizes = time.Minute, 48*time.Hour, share
		m, err := NewMonitor(pool, runner, nil, opts, func(string, string, ...any) {})
		if err != nil {
			t.Fatalf("NewMonitor: %v", err)
		}
		monitors, pools = append(monitors, m), append(pools, pool)
	}
	return monitors, pools, sizes
}

func tickAll(t *testing.T, ctx context.Context, ms []*Monitor) {
	t.Helper()
	var wg sync.WaitGroup
	for _, m := range ms {
		wg.Add(1)
		go func(m *Monitor) {
			defer wg.Done()
			if _, err := m.Sample(ctx); err != nil {
				t.Errorf("sample: %v", err)
			}
		}(m)
	}
	wg.Wait()
}

func TestFleetRunways_ShareOneSizeMeasurementPerCluster(t *testing.T) {
	_, ctx := livePool(t)
	ms, pools, counter := fleetMonitors(t, ctx, NewSizeShare())
	tickAll(t, ctx, ms)
	if n := counter.Load(); n != 1 {
		t.Fatalf("%d database-size measurements for one cluster in one pass, want 1", n)
	}
	a, b := sampledDatabaseBytes(t, ctx, pools[0]), sampledDatabaseBytes(t, ctx, pools[1])
	if a != b || a <= 0 {
		t.Fatalf("database_bytes samples %v and %v, want the same cluster value", a, b)
	}
	var direct float64
	if err := pools[0].QueryRow(ctx, `SELECT sum(pg_database_size(oid))::float8
		FROM pg_database WHERE datallowconn AND has_database_privilege(oid, 'CONNECT')`).
		Scan(&direct); err != nil {
		t.Fatalf("direct size: %v", err)
	}
	// Other test packages create databases concurrently: allow drift.
	if a < direct*0.5 || a > direct*2 {
		t.Fatalf("shared size %v far from the cluster's %v", a, direct)
	}
}

// Without a share every runtime measures for itself (today's behavior).
func TestFleetRunways_WithoutAShareEachRuntimeMeasures(t *testing.T) {
	_, ctx := livePool(t)
	ms, _, counter := fleetMonitors(t, ctx, nil)
	tickAll(t, ctx, ms)
	if n := counter.Load(); n != 2 {
		t.Fatalf("%d measurements without a share, want 2", n)
	}
}

// The next pass measures again.
func TestFleetRunways_NextPassMeasuresAgain(t *testing.T) {
	_, ctx := livePool(t)
	share := NewSizeShare()
	clock := &stepClock{now: time.Now()}
	share.now = clock.Now
	ms, _, counter := fleetMonitors(t, ctx, share)
	tickAll(t, ctx, ms)
	clock.advance(time.Minute)
	tickAll(t, ctx, ms)
	if n := counter.Load(); n != 2 {
		t.Fatalf("%d measurements over two passes, want 2", n)
	}
}
