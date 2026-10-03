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

// NewLatencyProxy builds the latency proxy over sage.query_store.
func NewLatencyProxy(pool *pgxpool.Pool, cfg ProxyConfig) Proxy {
	return &latencyProxy{pool: pool, cfg: cfg}
}

func (p *latencyProxy) Objective() Objective {
	return newProxyObjective(ProxyLatency, "database proxy: share of query_store intervals "+
		"whose p95 latency of the top queries by total time exceeds the threshold", p.cfg)
}

// baselineSpan is the history the relative threshold uses.
const latencyBaselineSpan = 7 * 24 * time.Hour

const capturesSQL = `/* pg_sage sre:slo_proxy */
SELECT DISTINCT captured_at FROM sage.query_store
WHERE captured_at > pg_catalog.now() - interval '30 minutes'
ORDER BY captured_at DESC LIMIT 2`

// deltasSQL lists the top queries of the interval ending at capture $1,
// and whether any query's counters reset (another statistics epoch, or
// fewer calls) in it. A query's sample is written only when its counters
// move, so its interval starts at its own last sample at or before the
// previous capture $2 (within $4, querystore.AnchorLookback), not at a
// row of $2 that an idle query does not have.
const deltasSQL = `/* pg_sage sre:slo_proxy */
SELECT (c.calls - p.calls)::float8, (c.total_exec_time - p.total_exec_time)::float8,
       bool_or(c.stats_epoch IS DISTINCT FROM p.stats_epoch OR c.calls < p.calls) OVER ()
FROM sage.query_store c
CROSS JOIN LATERAL (
    SELECT q.calls, q.total_exec_time, q.stats_epoch FROM sage.query_store q
    WHERE q.queryid = c.queryid AND q.captured_at <= $2
      AND q.captured_at >= $2 - $4::interval
    ORDER BY q.captured_at DESC, q.id DESC LIMIT 1) p
WHERE c.captured_at = $1
ORDER BY c.total_exec_time - p.total_exec_time DESC
LIMIT $3`

type queryDelta struct {
	calls, totalMs float64
}

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
	rows, err := p.pool.Query(ctx, capturesSQL)
	if err != nil {
		return 0, time.Time{}, ReasonSourceError
	}
	var caps []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return 0, time.Time{}, ReasonSourceError
		}
		caps = append(caps, at)
	}
	rows.Close()
	if rows.Err() != nil {
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

func (p *latencyProxy) deltas(ctx context.Context, cur, prev time.Time) ([]queryDelta, bool,
	error) {
	rows, err := p.pool.Query(ctx, deltasSQL, cur, prev, max(p.cfg.TopQueries, 1),
		querystore.AnchorLookback)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []queryDelta
	reset := false
	for rows.Next() {
		var d queryDelta
		var r bool
		if err := rows.Scan(&d.calls, &d.totalMs, &r); err != nil {
			return nil, false, err
		}
		reset = reset || r
		if d.calls > 0 && d.totalMs >= 0 {
			out = append(out, d)
		}
	}
	return out, reset, rows.Err()
}

// weightedP95 is the calls-weighted 95th percentile of the queries'
// interval mean latencies.
func weightedP95(ds []queryDelta) float64 {
	sort.Slice(ds, func(i, j int) bool {
		return ds[i].totalMs/ds[i].calls <
			ds[j].totalMs/ds[j].calls
	})
	total := 0.0
	for _, d := range ds {
		total += d.calls
	}
	target, cum := 0.95*total, 0.0
	for _, d := range ds {
		cum += d.calls
		if cum >= target {
			return d.totalMs / d.calls
		}
	}
	last := ds[len(ds)-1]
	return last.totalMs / last.calls
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
