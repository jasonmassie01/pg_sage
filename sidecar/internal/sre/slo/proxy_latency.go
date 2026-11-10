package slo

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// latencyProxy: each new query_store capture is one slice: the
// calls-weighted p95 of the interval mean latency of the top queries by
// interval total time. A slice is bad above the latency threshold. A
// statistics reset between captures is not a slice.
type latencyProxy struct {
	pool *pgxpool.Pool
	cfg  ProxyConfig

	mu   sync.Mutex
	last time.Time // newest capture already sliced
}

// NewLatencyProxy builds the latency proxy over sage.query_store, read in
// the pool's history store through the querystore readers.
func NewLatencyProxy(pool *pgxpool.Pool, cfg ProxyConfig) Proxy {
	return &latencyProxy{pool: pool, cfg: cfg}
}

func (p *latencyProxy) Objective() Objective {
	return newProxyObjective(ProxyLatency, "database proxy: share of query_store intervals "+
		"whose p95 latency of the top queries by total time exceeds the threshold", p.cfg)
}

// baselineSpan is the history the relative threshold uses.
const latencyBaselineSpan = 7 * 24 * time.Hour

// captureWindow is how recent the two captures a slice compares must be.
const captureWindow = 30 * time.Minute

func (p *latencyProxy) Slice(ctx context.Context, now time.Time, h History) ProxySlice {
	p95, latest, reason := p.measure(ctx)
	if reason != "" {
		return ProxySlice{Reason: reason}
	}
	p.mu.Lock()
	p.last = latest
	p.mu.Unlock()
	s := ProxySlice{Value: valuePtr(p95)}
	threshold, reason := p.threshold(ctx, now, h)
	if reason != "" {
		s.Reason = reason
		return s
	}
	s.Eligible = 1
	if p95 > threshold {
		s.Bad = 1
	}
	return s
}

// measure reads the newest interval's p95, or why there is none.
func (p *latencyProxy) measure(ctx context.Context) (float64, time.Time, string) {
	caps, err := querystore.RecentCaptures(ctx, p.pool, captureWindow, 2)
	if err != nil {
		return 0, time.Time{}, ReasonSourceError
	}
	p.mu.Lock()
	seen := len(caps) > 0 && !caps[0].After(p.last)
	p.mu.Unlock()
	if len(caps) < 2 || seen {
		return 0, time.Time{}, ReasonNoNewCapture
	}
	deltas, reset, err := p.deltas(ctx, caps[0], caps[1])
	switch {
	case err != nil:
		return 0, time.Time{}, ReasonSourceError
	case reset:
		return 0, caps[0], ReasonStatsReset
	case len(deltas) == 0:
		return 0, caps[0], ReasonNoNewCapture
	}
	return weightedP95(deltas), caps[0], ""
}

// deltas reads the interval's top queries (querystore.CaptureDeltas, which
// owns the anchor and epoch rules) and keeps those that ran.
func (p *latencyProxy) deltas(ctx context.Context, cur, prev time.Time) ([]querystore.Delta,
	bool, error) {
	all, reset, err := querystore.CaptureDeltas(ctx, p.pool, cur, prev,
		max(p.cfg.TopQueries, 1))
	if err != nil {
		return nil, false, err
	}
	out := all[:0]
	for _, d := range all {
		if d.Calls > 0 && d.TotalMs >= 0 {
			out = append(out, d)
		}
	}
	return out, reset, nil
}

// weightedP95 is the calls-weighted 95th percentile of the queries'
// interval mean latencies.
func weightedP95(ds []querystore.Delta) float64 {
	sort.Slice(ds, func(i, j int) bool {
		return ds[i].TotalMs/ds[i].Calls <
			ds[j].TotalMs/ds[j].Calls
	})
	total := 0.0
	for _, d := range ds {
		total += d.Calls
	}
	target, cum := 0.95*total, 0.0
	for _, d := range ds {
		cum += d.Calls
		if cum >= target {
			return d.TotalMs / d.Calls
		}
	}
	last := ds[len(ds)-1]
	return last.TotalMs / last.Calls
}

// threshold is the configured absolute threshold, or the relative one
// from the stored history (unknown until it has enough samples).
func (p *latencyProxy) threshold(ctx context.Context, now time.Time, h History) (float64,
	string) {
	if p.cfg.LatencyThresholdMs > 0 {
		return p.cfg.LatencyThresholdMs, ""
	}
	median, n, err := h.Baseline(ctx, now.Add(-latencyBaselineSpan))
	switch {
	case err != nil:
		return 0, ReasonSourceError
	case n < p.cfg.MinBaselineSamples || math.IsNaN(median):
		return 0, ReasonBaselineBuilding
	}
	return math.Max(p.cfg.LatencyFloorMs, p.cfg.LatencyFactor*median), ""
}
