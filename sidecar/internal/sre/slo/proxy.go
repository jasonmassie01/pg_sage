package slo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database proxy SLIs (AI-SRE-SPEC §8): used when no app SLI exists and
// always labeled proxies. They need no external setup. Latency,
// connection and replication are time-slice SLIs (each engine tick is one
// eligible event, bad when the database breached its threshold); the
// error proxy counts server-class log errors per transaction.
const (
	ProxyLatency        = "db_latency"
	ProxyErrors         = "db_errors"
	ProxyConnections    = "db_connection_refusal"
	ProxyReplicationLag = "db_replication_lag"
)

// History is a proxy's own stored gauge history.
type History interface {
	// Baseline is the median gauge value since a time and how many
	// values there were.
	Baseline(ctx context.Context, since time.Time) (float64, int, error)
}

// ProxySlice is one proxy observation: increments of bad and eligible
// events, an optional gauge value to keep, and why no eligible event was
// produced (when none was).
type ProxySlice struct {
	Bad      float64
	Eligible float64
	Value    *float64
	Reason   string
}

// Proxy is a database proxy SLI.
type Proxy interface {
	Objective() Objective
	Slice(ctx context.Context, now time.Time, h History) ProxySlice
}

// ProxyConfig tunes the proxies. A zero LatencyThresholdMs means a
// relative latency threshold: LatencyFactor times the database's own
// median interval p95 over the last week, never under LatencyFloorMs.
type ProxyConfig struct {
	Target               float64
	Window               time.Duration
	LatencyThresholdMs   float64
	LatencyFactor        float64
	LatencyFloorMs       float64
	TopQueries           int
	ReplicationLagBudget time.Duration
	MinBaselineSamples   int
}

// DefaultProxyConfig is the AI-DBA default: on, self-calibrating.
func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{Target: 0.99, Window: 30 * 24 * time.Hour, LatencyFactor: 3,
		LatencyFloorMs: 50, TopQueries: 20, ReplicationLagBudget: time.Minute,
		MinBaselineSamples: 30}
}

// proxyMinEligible is the least slices a window needs (a 5-minute window
// at one-minute ticks has five).
const proxyMinEligible = 3

func newProxyObjective(name, description string, c ProxyConfig) Objective {
	return Objective{Name: name, Kind: KindProxy, Source: SourceProxy, Proxy: name,
		Description: description, Target: c.Target, Window: c.Window,
		MinEligible: proxyMinEligible, StaleAfter: DefaultStaleAfter}
}

func valuePtr(v float64) *float64 { return &v }

// connectionProxy: a slice is bad when every client connection slot
// (max_connections minus the reserved ones) is in use, so new
// connections are refused.
type connectionProxy struct {
	pool *pgxpool.Pool
	cfg  ProxyConfig
}

// NewConnectionProxy builds the connection-refusal proxy.
func NewConnectionProxy(pool *pgxpool.Pool, cfg ProxyConfig) Proxy {
	return &connectionProxy{pool: pool, cfg: cfg}
}

func (p *connectionProxy) Objective() Objective {
	return newProxyObjective(ProxyConnections, "database proxy: share of minutes with every "+
		"client connection slot in use (new connections refused)", p.cfg)
}

const connectionSQL = `/* pg_sage sre:slo_proxy */
SELECT (SELECT count(*) FROM pg_catalog.pg_stat_activity
        WHERE backend_type = 'client backend')::float8,
       (pg_catalog.current_setting('max_connections')::int
        - pg_catalog.current_setting('superuser_reserved_connections')::int
        - COALESCE(pg_catalog.current_setting('reserved_connections', true), '0')::int)::float8`

func (p *connectionProxy) Slice(ctx context.Context, _ time.Time, _ History) ProxySlice {
	var used, slots float64
	if err := p.pool.QueryRow(ctx, connectionSQL).Scan(&used, &slots); err != nil {
		return ProxySlice{Reason: ReasonSourceError}
	}
	if slots <= 0 {
		return ProxySlice{Reason: ReasonSourceError}
	}
	s := ProxySlice{Eligible: 1, Value: valuePtr(used / slots)}
	if used >= slots {
		s.Bad = 1
	}
	return s
}

// replicationProxy: a slice is bad when the largest replay lag of a
// standby (or of this standby) exceeds the lag budget. A primary without
// standbys has nothing to measure.
type replicationProxy struct {
	pool *pgxpool.Pool
	cfg  ProxyConfig
}

// NewReplicationLagProxy builds the replication-lag budget proxy.
func NewReplicationLagProxy(pool *pgxpool.Pool, cfg ProxyConfig) Proxy {
	return &replicationProxy{pool: pool, cfg: cfg}
}

func (p *replicationProxy) Objective() Objective {
	return newProxyObjective(ProxyReplicationLag, "database proxy: share of minutes with "+
		"replication replay lag over the lag budget", p.cfg)
}

const replicationSQL = `/* pg_sage sre:slo_proxy */
SELECT pg_catalog.pg_is_in_recovery(),
       (SELECT count(*) FROM pg_catalog.pg_stat_replication)::int,
       COALESCE((SELECT max(EXTRACT(EPOCH FROM replay_lag))
                 FROM pg_catalog.pg_stat_replication), 0)::float8,
       CASE WHEN pg_catalog.pg_is_in_recovery() THEN
           CASE WHEN pg_catalog.pg_last_wal_receive_lsn()
                     = pg_catalog.pg_last_wal_replay_lsn() THEN 0
                ELSE COALESCE(EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
                     - pg_catalog.pg_last_xact_replay_timestamp()), 0) END
       END::float8`

func (p *replicationProxy) Slice(ctx context.Context, _ time.Time, _ History) ProxySlice {
	var standby bool
	var replicas int
	var primaryLag float64
	var standbyLag *float64
	if err := p.pool.QueryRow(ctx, replicationSQL).Scan(&standby, &replicas, &primaryLag,
		&standbyLag); err != nil {
		return ProxySlice{Reason: ReasonSourceError}
	}
	lag := primaryLag
	switch {
	case standby && standbyLag != nil:
		lag = *standbyLag
	case !standby && replicas == 0:
		return ProxySlice{Reason: ReasonNoReplicas}
	}
	s := ProxySlice{Eligible: 1, Value: valuePtr(lag)}
	if lag > p.cfg.ReplicationLagBudget.Seconds() {
		s.Bad = 1
	}
	return s
}
