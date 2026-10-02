package slo

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrorCounter drains the server-class errors counted since the last
// drain; ok is false when no log source is available.
type ErrorCounter interface {
	DrainErrors() (int64, bool)
}

// ServerErrorClass reports whether a SQLSTATE is a server-side error
// class (the database's health, not the application's input): resources
// (53), operator intervention such as timeouts and shutdown (57), system
// errors (58), internal errors (XX), deadlocks and serialization
// failures (40), lock-not-available (55P03) and connection exceptions
// (08). Data, constraint and syntax errors are the application's.
func ServerErrorClass(sqlstate string) bool {
	if len(sqlstate) != 5 {
		return false
	}
	switch sqlstate[:2] {
	case "53", "57", "58", "XX", "40", "08":
		return true
	}
	return strings.EqualFold(sqlstate, "55P03")
}

// errorProxy: server-class log errors per transaction in each tick. The
// first tick only records the transaction baseline.
type errorProxy struct {
	pool    *pgxpool.Pool
	counter ErrorCounter
	cfg     ProxyConfig

	mu   sync.Mutex
	prev float64
	have bool
}

// NewErrorProxy builds the error-class proxy; a nil counter (no log
// source) leaves it unknown (source_unavailable).
func NewErrorProxy(pool *pgxpool.Pool, counter ErrorCounter, cfg ProxyConfig) Proxy {
	return &errorProxy{pool: pool, counter: counter, cfg: cfg}
}

func (p *errorProxy) Objective() Objective {
	return newProxyObjective(ProxyErrors, "database proxy: server-class errors in the server "+
		"log per transaction (needs log access)", p.cfg)
}

const xactsSQL = `/* pg_sage sre:slo_proxy */
SELECT (xact_commit + xact_rollback)::float8 FROM pg_catalog.pg_stat_database
WHERE datname = pg_catalog.current_database()`

func (p *errorProxy) Slice(ctx context.Context, _ time.Time, _ History) ProxySlice {
	if p.counter == nil {
		return ProxySlice{Reason: ReasonSourceUnavailable}
	}
	errs, ok := p.counter.DrainErrors()
	if !ok {
		return ProxySlice{Reason: ReasonSourceUnavailable}
	}
	var xacts float64
	if err := p.pool.QueryRow(ctx, xactsSQL).Scan(&xacts); err != nil {
		return ProxySlice{Reason: ReasonSourceError}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, had := p.prev, p.have
	p.prev, p.have = xacts, true
	switch {
	case !had:
		return ProxySlice{Reason: ReasonBaselineBuilding}
	case xacts < prev:
		return ProxySlice{Reason: ReasonStatsReset}
	}
	delta := xacts - prev
	return ProxySlice{Eligible: delta, Bad: math.Min(float64(errs), delta)}
}
