package collector

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Collector runs periodic stats collection against the target database.
type Collector struct {
	pool         *pgxpool.Pool
	cfg          *config.Config
	breaker      *CircuitBreaker
	mu           sync.RWMutex
	latest       *Snapshot
	previous     *Snapshot
	pgVersionNum int // e.g. 170009 for PG 17.9
	blkTime      *blockTimeExprs
	logFn        func(string, string, ...any)

	// skipConfigSnapshots is set before Run when no advisor will consume
	// the configuration snapshot.
	skipConfigSnapshots bool
}

// New creates a Collector wired to the given pool and config.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	pgVersionNum int,
	logFn func(string, string, ...any),
) *Collector {
	return &Collector{
		pool:         pool,
		cfg:          cfg,
		pgVersionNum: pgVersionNum,
		breaker: NewCircuitBreaker(
			cfg.Safety.CPUCeilingPct,
			cfg.Safety.BackoffConsecutiveSkips,
		),
		logFn: logFn,
	}
}

// WithoutConfigSnapshots stops the advisor's per-cycle configuration
// snapshot (pg_settings and every table's reloptions). The runtime calls
// it, before Run, when no advisor will run, e.g. without a usable LLM.
func (c *Collector) WithoutConfigSnapshots() { c.skipConfigSnapshots = true }

// CollectsConfigSnapshots reports whether each cycle gathers the
// advisor's configuration snapshot.
func (c *Collector) CollectsConfigSnapshots() bool {
	return c.cfg.Advisor.Enabled && !c.skipConfigSnapshots
}

// Run starts the collection loop, blocking until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	interval := c.cfg.Collector.Interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.logFn("INFO", "collector started, interval=%s", interval)

	for {
		select {
		case <-ctx.Done():
			c.logFn("INFO", "collector stopped")
			return
		case <-ticker.C:
			c.cycle(ctx, ticker)
		}
	}
}

func (c *Collector) cycle(ctx context.Context, ticker *time.Ticker) {
	if c.breaker.ShouldSkip(ctx, timedCatalogQuerier{collector: c}) {
		if c.breaker.IsDormant() {
			c.logFn("WARN", "circuit breaker dormant, using dormant interval")
			ticker.Reset(c.cfg.Safety.DormantInterval())
		} else {
			c.logFn("WARN", "circuit breaker skip, db load too high")
		}
		return
	}

	// Restore normal interval if we were dormant.
	if c.breaker.IsDormant() {
		ticker.Reset(c.cfg.Collector.Interval())
	}

	snap, err := c.collect(ctx)
	if err != nil {
		c.logFn("ERROR", "collection failed: %v", err)
		return
	}

	c.mu.Lock()
	c.previous = c.latest
	c.latest = snap
	c.mu.Unlock()

	c.breaker.RecordSuccess()

	if err := c.persist(ctx, snap); err != nil {
		c.logFn("ERROR", "snapshot persist failed: %v", err)
	}

	c.recordQueryStore(ctx, snap)
}

// LatestSnapshot returns the most recent snapshot (thread-safe).
func (c *Collector) LatestSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// PreviousSnapshot returns the snapshot before the latest (thread-safe).
func (c *Collector) PreviousSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.previous
}

// collect gathers all stats categories into a single Snapshot.
func (c *Collector) collect(ctx context.Context) (*Snapshot, error) {
	now := time.Now().UTC()
	snap := &Snapshot{CollectedAt: now}

	var err error

	// Read the epoch before the counters: a reset in between leaves the
	// old epoch on reset counters, which the counter-decrease check and
	// the next cycle's epoch change both expose.
	snap.StatsEpoch = c.collectStatementsEpoch(ctx)
	if snap.Queries, err = c.collectQueries(ctx); err != nil {
		return nil, err
	}
	if snap.Tables, err = c.collectTables(ctx); err != nil {
		return nil, err
	}
	if snap.Indexes, err = c.collectIndexes(ctx); err != nil {
		return nil, err
	}
	if snap.ForeignKeys, err = c.collectForeignKeys(ctx); err != nil {
		return nil, err
	}
	if snap.System, err = c.collectSystem(ctx); err != nil {
		return nil, err
	}
	if snap.Locks, err = c.collectLocks(ctx); err != nil {
		return nil, err
	}
	if snap.Sequences, err = c.collectSequences(ctx); err != nil {
		return nil, err
	}
	// Non-fatal: a single replication-slot quirk (e.g. an unreserved
	// slot, or a hot standby) must not abort the entire snapshot cycle,
	// which would silently halt all stats collection (H1/H2). Match the
	// WARN-and-continue behavior of the sibling collectors below.
	if snap.Replication, err = c.collectReplication(ctx); err != nil {
		c.logFn("WARN", "replication collection failed: %v", err)
	}
	// pg_stat_io (PG16+)
	if c.pgVersionNum >= 160000 {
		if snap.IO, err = c.collectIO(ctx); err != nil {
			c.logFn("WARN", "pg_stat_io collection failed: %v", err)
			// Non-fatal — continue without IO stats.
		}
	}
	// Prepared transactions (2PC) — invisible to pg_stat_activity,
	// hold xmin and locks indefinitely.
	if snap.PreparedXacts, err = c.collectPreparedXacts(ctx); err != nil {
		c.logFn("WARN", "prepared xacts collection failed: %v", err)
	}
	// Partition inheritance
	if snap.Partitions, err = c.collectPartitions(ctx); err != nil {
		c.logFn("WARN", "partition collection failed: %v", err)
	}
	// Config data for advisor features
	if c.CollectsConfigSnapshots() {
		querier := timedCatalogQuerier{collector: c}
		if snap.ConfigData, err = collectConfigSnapshot(ctx, querier); err != nil {
			c.logFn("WARN", "config snapshot collection failed: %v", err)
		}
	}

	// Collect pg_stat_statements.max for capacity monitoring.
	snap.System.StatStatementsMax = c.collectStatStatementsMax(ctx)

	c.markStatsReset(snap)
	return snap, nil
}

// markStatsReset flags snap when pg_stat_statements was reset since the
// previous snapshot: the statistics epoch changed (reliable even after
// counters regrew), or most shared counters fell sharply.
func (c *Collector) markStatsReset(snap *Snapshot) {
	c.mu.RLock()
	prev := c.latest
	c.mu.RUnlock()
	if prev == nil {
		return
	}
	if epochChanged(prev.StatsEpoch, snap.StatsEpoch) ||
		detectStatsReset(snap.Queries, prev.Queries) {
		snap.StatsReset = true
		c.logFn("WARN", "pg_stat_statements reset detected")
	}
}
