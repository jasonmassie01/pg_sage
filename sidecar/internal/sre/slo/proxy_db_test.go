package slo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database proxy SLIs (AI-SRE-SPEC §8) against a real PostgreSQL: they
// need no external setup, are always labeled proxies, and say why they
// cannot measure instead of reporting a healthy zero.

type fakeHistory struct {
	median float64
	n      int
	err    error
}

func (h fakeHistory) Baseline(context.Context, time.Time) (float64, int, error) {
	return h.median, h.n, h.err
}

func TestDefaultProxyConfig(t *testing.T) {
	c := DefaultProxyConfig()
	if c.Target != 0.99 || c.Window != 30*24*time.Hour || c.LatencyFactor != 3 ||
		c.LatencyFloorMs != 50 || c.TopQueries != 20 ||
		c.ReplicationLagBudget != time.Minute || c.MinBaselineSamples != 30 ||
		c.LatencyThresholdMs != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestProxyObjectives_AreProxies(t *testing.T) {
	pool, _ := livePool(t)
	cfg := DefaultProxyConfig()
	for _, p := range []Proxy{NewConnectionProxy(pool, cfg), NewReplicationLagProxy(pool, cfg),
		NewLatencyProxy(pool, cfg), NewErrorProxy(pool, nil, cfg)} {
		o := p.Objective()
		if o.Kind != KindProxy || o.Source != SourceProxy || o.Target != 0.99 ||
			o.Proxy != o.Name || o.Validate() != nil {
			t.Errorf("proxy objective = %+v (validate: %v)", o, o.Validate())
		}
	}
}

func TestConnectionProxy_SlotsAvailable(t *testing.T) {
	pool, ctx := livePool(t)
	p := NewConnectionProxy(pool, DefaultProxyConfig())
	if p.Objective().Name != ProxyConnections {
		t.Fatalf("name = %s", p.Objective().Name)
	}
	s := p.Slice(ctx, time.Now(), fakeHistory{})
	if s.Reason != "" || s.Eligible != 1 || s.Bad != 0 || s.Value == nil ||
		*s.Value <= 0 || *s.Value >= 1 {
		t.Fatalf("slice = %+v (value %v)", s, s.Value)
	}
}

// A primary without standbys has no replication lag to measure.
func TestReplicationLagProxy_NoReplicas(t *testing.T) {
	pool, ctx := livePool(t)
	p := NewReplicationLagProxy(pool, DefaultProxyConfig())
	s := p.Slice(ctx, time.Now(), fakeHistory{})
	if s.Reason != ReasonNoReplicas || s.Eligible != 0 || s.Bad != 0 {
		t.Fatalf("slice = %+v", s)
	}
}

// insertCapture writes one query_store capture of three queries.
func insertCapture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, age time.Duration,
	epoch string, calls [3]int64, totals [3]float64) {
	t.Helper()
	// One capture shares one captured_at, as the collector's batch does.
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		    total_exec_time, mean_exec_time, stats_epoch)
		SELECT now() - $1 * interval '1 second', q.id, q.calls, q.total, 0, $8::timestamptz
		FROM (VALUES (9001, $2::int8, $5::float8), (9002, $3::int8, $6::float8),
		             (9003, $4::int8, $7::float8)) AS q(id, calls, total)`,
		age.Seconds(), calls[0], calls[1], calls[2], totals[0], totals[1], totals[2], epoch)
	if err != nil {
		t.Fatalf("insert capture: %v", err)
	}
}

func clearCaptures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_store
		WHERE queryid IN (9001, 9002, 9003)`); err != nil {
		t.Fatal(err)
	}
}

const epoch = "2026-09-01T00:00:00Z"

// The interval p95 is the calls-weighted 95th percentile of each top
// query's interval mean latency: here q1 10 ms (100 calls), q2 200 ms
// (10 calls), q3 5 ms (1 call) -> 200 ms.
func TestLatencyProxy_IntervalP95AgainstBaseline(t *testing.T) {
	pool, ctx := livePool(t)
	clearCaptures(t, ctx, pool)
	insertCapture(t, ctx, pool, 120*time.Second, epoch, [3]int64{1000, 50, 7},
		[3]float64{10000, 1000, 70})
	insertCapture(t, ctx, pool, 60*time.Second, epoch, [3]int64{1100, 60, 8},
		[3]float64{11000, 3000, 75})
	p := NewLatencyProxy(pool, DefaultProxyConfig())
	s := p.Slice(ctx, time.Now(), fakeHistory{median: 10, n: 100})
	if s.Value == nil || *s.Value != 200 || s.Eligible != 1 || s.Bad != 1 || s.Reason != "" {
		t.Fatalf("slice = %+v value=%v, want a bad 200 ms slice (threshold 50)", s, s.Value)
	}
	again := p.Slice(ctx, time.Now(), fakeHistory{median: 10, n: 100})
	if again.Eligible != 0 || again.Reason != ReasonNoNewCapture {
		t.Fatalf("same capture twice: %+v", again)
	}
	// 200 ms is under 3x a 100 ms baseline: a good slice.
	fresh := NewLatencyProxy(pool, DefaultProxyConfig())
	good := fresh.Slice(ctx, time.Now(), fakeHistory{median: 100, n: 100})
	if good.Eligible != 1 || good.Bad != 0 {
		t.Fatalf("under the relative threshold: %+v", good)
	}
}

// Before enough history, the relative threshold is unknown: the value is
// still recorded (it builds the baseline) but the slice is not eligible.
func TestLatencyProxy_BaselineBuildingAndAbsoluteMode(t *testing.T) {
	pool, ctx := livePool(t)
	clearCaptures(t, ctx, pool)
	insertCapture(t, ctx, pool, 120*time.Second, epoch, [3]int64{1000, 50, 7},
		[3]float64{10000, 1000, 70})
	insertCapture(t, ctx, pool, 60*time.Second, epoch, [3]int64{1100, 60, 8},
		[3]float64{11000, 3000, 75})
	s := NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, time.Now(),
		fakeHistory{median: 10, n: 29})
	if s.Reason != ReasonBaselineBuilding || s.Eligible != 0 || s.Value == nil {
		t.Fatalf("29 baseline samples: %+v", s)
	}
	s = NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, time.Now(),
		fakeHistory{err: errors.New("store down")})
	if s.Reason != ReasonSourceError || s.Eligible != 0 {
		t.Fatalf("baseline error: %+v", s)
	}
	abs := DefaultProxyConfig()
	abs.LatencyThresholdMs = 250
	s = NewLatencyProxy(pool, abs).Slice(ctx, time.Now(), fakeHistory{})
	if s.Eligible != 1 || s.Bad != 0 || s.Reason != "" {
		t.Fatalf("absolute 250 ms threshold, p95 200 ms: %+v", s)
	}
}

// A statistics reset between captures (another epoch) is not a slice.
func TestLatencyProxy_EpochChangeAndNoCaptures(t *testing.T) {
	pool, ctx := livePool(t)
	clearCaptures(t, ctx, pool)
	s := NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, time.Now(), fakeHistory{})
	if s.Eligible != 0 || s.Reason != ReasonNoNewCapture {
		t.Fatalf("no captures: %+v", s)
	}
	insertCapture(t, ctx, pool, 120*time.Second, epoch, [3]int64{1000, 50, 7},
		[3]float64{10000, 1000, 70})
	insertCapture(t, ctx, pool, 60*time.Second, "2026-09-30T00:00:00Z", [3]int64{5, 5, 5},
		[3]float64{50, 50, 50})
	s = NewLatencyProxy(pool, DefaultProxyConfig()).Slice(ctx, time.Now(),
		fakeHistory{median: 10, n: 100})
	if s.Eligible != 0 || s.Reason != ReasonStatsReset {
		t.Fatalf("epoch change: %+v", s)
	}
}

type fakeErrors struct {
	n  int64
	ok bool
}

func (f *fakeErrors) DrainErrors() (int64, bool) { return f.n, f.ok }

// Error-class proxy: server-class log errors per transaction. The first
// slice only sets the transaction baseline; errors never exceed the
// transactions they are counted against.
func TestErrorProxy(t *testing.T) {
	pool, ctx := livePool(t)
	counter := &fakeErrors{n: 0, ok: true}
	p := NewErrorProxy(pool, counter, DefaultProxyConfig())
	first := p.Slice(ctx, time.Now(), fakeHistory{})
	if first.Eligible != 0 || first.Reason != ReasonBaselineBuilding {
		t.Fatalf("first slice: %+v", first)
	}
	for i := 0; i < 20; i++ {
		if _, err := pool.Exec(ctx, `SELECT 1`); err != nil {
			t.Fatal(err)
		}
	}
	counter.n = 3
	// Transaction counts reach pg_stat_database asynchronously (backends
	// flush their statistics when idle), so the slice is polled until the
	// transactions above are visible.
	var s ProxySlice
	for i := 0; i < 40; i++ {
		s = p.Slice(ctx, time.Now(), fakeHistory{})
		if s.Eligible >= 3 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if s.Eligible < 3 || s.Bad != 3 || s.Reason != "" {
		t.Fatalf("slice = %+v", s)
	}
	counter.n = 1 << 40
	s = p.Slice(ctx, time.Now(), fakeHistory{})
	if s.Bad > s.Eligible {
		t.Fatalf("errors exceed transactions: %+v", s)
	}
	counter.ok = false
	if s := p.Slice(ctx, time.Now(), fakeHistory{}); s.Reason != ReasonSourceUnavailable ||
		s.Eligible != 0 {
		t.Fatalf("no log source: %+v", s)
	}
	none := NewErrorProxy(pool, nil, DefaultProxyConfig())
	if s := none.Slice(ctx, time.Now(), fakeHistory{}); s.Reason != ReasonSourceUnavailable {
		t.Fatalf("nil counter: %+v", s)
	}
}

// A closed pool is a source error, never a healthy slice.
func TestProxies_ClosedPool(t *testing.T) {
	pool, ctx := livePool(t)
	pool.Close()
	cfg := DefaultProxyConfig()
	for _, p := range []Proxy{NewConnectionProxy(pool, cfg), NewReplicationLagProxy(pool, cfg),
		NewLatencyProxy(pool, cfg)} {
		s := p.Slice(ctx, time.Now(), fakeHistory{median: 1, n: 100})
		if s.Reason != ReasonSourceError || s.Eligible != 0 {
			t.Errorf("%s on a closed pool: %+v", p.Objective().Name, s)
		}
	}
}
