package collector

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

func (c *Collector) collectSystem(ctx context.Context) (SystemStats, error) {
	sql := systemStatsSQL14
	if c.pgVersionNum >= 170000 {
		sql = systemStatsSQL17
	}
	s, err := c.scanSystemStats(ctx, sql)
	var pgErr *pgconn.PgError
	if c.pgVersionNum >= 170000 && errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		s, err = c.scanSystemStats(ctx, systemStatsSQL14)
	}
	if err == nil {
		s.DBSizeBytes = c.databaseSize(ctx)
	}
	return s, err
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
