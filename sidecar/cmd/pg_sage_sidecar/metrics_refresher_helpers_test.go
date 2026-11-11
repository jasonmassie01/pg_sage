package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/value"
)

// withMetricsRegistry gives the test its own refresher registry and pool
// health, restored when the test ends.
func withMetricsRegistry(t *testing.T) *metricsRegistry {
	t.Helper()
	old := processMetrics
	oldHealth := poolHealth.load()
	processMetrics = newMetricsRegistry()
	poolHealth.reset()
	t.Cleanup(func() {
		processMetrics = old
		poolHealth.restore(oldHealth)
	})
	return processMetrics
}

// refreshRegisteredMetrics runs one refresh of every registered refresher,
// as their loops would at the next tick.
func refreshRegisteredMetrics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, m := range processMetrics.refreshers() {
		m.refresh(ctx)
	}
}

// registerStandaloneMetrics wires what a standalone runtime starts: the
// pool health result and a refresher with the standalone sources.
func registerStandaloneMetrics(t *testing.T, name string, p *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkPoolHealth(ctx, p)
	m := newMetricsRefresher(name, fixedInterval(time.Hour),
		valueMetricsSource(name, p), optimizerMetricsSource(p))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	m.refresh(ctx)
}

// registerValueRefresher registers a refresher reading one value ledger.
func registerValueRefresher(t *testing.T, name string, p *pgxpool.Pool) *metricsRefresher {
	t.Helper()
	m := newMetricsRefresher(name, fixedInterval(time.Hour), valueMetricsSource(name, p))
	processMetrics.register(m)
	t.Cleanup(func() { processMetrics.unregister(m) })
	return m
}

// renderMetricsBody scrapes /metrics without a *testing.T, for goroutines.
func renderMetricsBody() string {
	w := httptest.NewRecorder()
	handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

func fixedInterval(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}

// hungPool is a pool whose dialer never connects, like a blackholed host.
// The dialer gives up when the test ends so Close never waits on it.
func hungPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://sage@192.0.2.1:5432/hung?sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	release := make(chan struct{})
	cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("test finished")
		}
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(p.Close)
	t.Cleanup(func() { close(release) })
	return p
}

// fakeValueSource reports fixed ledger values for a healthy database.
func fakeValueSource(identity string, toil float64, incidents int64) metricsSource {
	return metricsSource{name: metricsSourceValue,
		read: func(_ context.Context, s *metricsSnapshot) error {
			s.value = value.MetricsRead{Identity: identity,
				ToilMinutesByFeature: map[string]float64{"analyze_table": toil},
				IncidentsByKind:      map[string]int64{"xid_wraparound": incidents}}
			return nil
		}}
}

// fakeSLOSource reports one SLO whose remaining budget is budget.
func fakeSLOSource(budget float64) metricsSource {
	return sloMetricsSource(sloReaderFunc(func(context.Context) ([]slo.Status, error) {
		b := budget
		return []slo.Status{{Name: "checkout", Kind: slo.KindApp, State: slo.StateOK,
			BudgetRemaining: &b}}, nil
	}))
}

type sloReaderFunc func(context.Context) ([]slo.Status, error)

func (f sloReaderFunc) Statuses(ctx context.Context) ([]slo.Status, error) { return f(ctx) }

// blockingSLOReader never answers before its context ends.
var blockingSLOReader = sloReaderFunc(func(ctx context.Context) ([]slo.Status, error) {
	<-ctx.Done()
	return nil, ctx.Err()
})

// ioCounter counts every query and connection acquire on a pool.
type ioCounter struct{ n atomic.Int64 }

func (c *ioCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	_ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *ioCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *ioCounter) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool,
	_ pgxpool.TraceAcquireStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *ioCounter) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}
