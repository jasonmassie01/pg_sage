package collector

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// collectSystem reads the system stats with the live version's SQL. The
// other major version's SQL is the fallback, both ways (a PG17 server has
// no checkpoints_timed, 42703; an older one no pg_stat_checkpointer,
// 42P01): the version moved since the probe, so the next cycle re-probes.
func (c *Collector) collectSystem(ctx context.Context) (SystemStats, error) {
	sql, other := systemStatsSQL14, systemStatsSQL17
	if c.Capabilities().PGVersionNum >= 170000 {
		sql, other = other, sql
	}
	s, err := c.scanSystemStats(ctx, sql)
	if otherMajorVersion(err) {
		c.invalidateCapabilities()
		s, err = c.scanSystemStats(ctx, other)
	}
	if err == nil {
		s.DBSizeBytes = c.databaseSize(ctx)
	}
	return s, err
}

// otherMajorVersion reports an undefined column or relation: the system
// SQL of the other major version.
func otherMajorVersion(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42703" || pgErr.Code == "42P01")
}

// scanSystemStats reads one system stats row. A NULL cache hit ratio (no
// block accesses yet) becomes CacheHitRatioUnknown, never 0 (G1-B08).
func (c *Collector) scanSystemStats(ctx context.Context, sql string) (SystemStats, error) {
	var s SystemStats
	var ratio *float64
	err := c.catalogQueryRow(ctx, sql).Scan(
		&s.ActiveBackends, &s.IdleInTransaction,
		&s.TotalBackends, &s.MaxConnections,
		&ratio, &s.Deadlocks,
		&s.BlkReadTime, &s.BlkWriteTime,
		&s.TotalCheckpoints, &s.IsReplica,
	)
	s.CacheHitRatio = CacheHitRatioUnknown
	if ratio != nil {
		s.CacheHitRatio = *ratio
	}
	if err == nil {
		s.RelationStatsEpoch = c.collectRelationStatsEpoch(ctx)
	}
	return s, err
}

// databaseSize is the database's size, measured every DBSizeRefreshInterval
// (measured.md M7: a stat() of every file, 147-177 ms on lifeos, twice a
// minute). A failed measurement keeps the last size (0 = unknown, stored
// as null) and is retried at the next interval; it never fails the
// system stats, the snapshot's spine (it timed out on lifeos and lost the
// whole snapshot).
func (c *Collector) databaseSize(ctx context.Context) int64 {
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	now := c.clock()
	if !c.dbSize.due(now, c.dbSizeEvery) {
		return c.dbSize.bytes
	}
	c.dbSize.at = now
	var size int64
	if err := c.catalogQueryRow(ctx, databaseSizeSQL).Scan(&size); err != nil {
		c.logFn("WARN", "collector: database size unavailable this cycle "+
			"(keeping %d bytes, retry in %s): %v", c.dbSize.bytes, c.dbSizeEvery, err)
		return c.dbSize.bytes
	}
	c.dbSize.bytes = size
	return size
}
