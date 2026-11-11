package collector

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// collectSystem reads the system stats with the live version's SQL. The
// other major version's SQL is the fallback, both ways (a PG17 server has
// no checkpoints_timed, 42703; an older one no pg_stat_checkpointer,
// 42P01): the version moved since the probe, so the next cycle re-probes.
func (c *Collector) collectSystem(ctx context.Context) (SystemStats, error) {
	version := c.Capabilities().PGVersionNum
	s, err := c.scanSystemStats(ctx, systemStatsSQLFor(version))
	if fallback := systemStatsFallback(version, err); fallback != "" {
		c.invalidateCapabilities()
		s, err = c.scanSystemStats(ctx, fallback)
	}
	if err == nil {
		s.DBSizeBytes = c.databaseSize(ctx)
		c.readWALPosition(ctx, &s)
		c.readWALStats(ctx, &s)
	}
	return s, err
}

// walInsertLSNSQL is the WAL insert position in bytes on a primary; NULL
// on a standby, which replays WAL it did not generate (and where
// pg_current_wal_lsn() fails).
const walInsertLSNSQL = sageTag + `SELECT CASE WHEN pg_is_in_recovery() THEN NULL
  ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint END`

// readWALPosition sets the WAL insert position and the instant it was
// read (the midpoint of the read), which the WAL rate divides by (rca-03).
// It never fails the system stats: an unreadable position stays zero, an
// unknown WAL rate for this cycle.
func (c *Collector) readWALPosition(ctx context.Context, s *SystemStats) {
	start := time.Now().UTC()
	var lsn *int64
	if err := c.catalogQueryRow(ctx, walInsertLSNSQL).Scan(&lsn); err != nil {
		c.logFn("WARN", "collector: WAL position unavailable this cycle "+
			"(no WAL rate signal until it is readable): %v", err)
		return
	}
	if lsn == nil {
		return
	}
	end := time.Now().UTC()
	s.WALLSN, s.WALReadAt = *lsn, start.Add(end.Sub(start)/2)
}

// walStatsSQL is the cluster's WAL generated since the epoch of
// pg_stat_wal (PG 14+): the later of its stats_reset and the postmaster
// start, as the checkpoint epoch is.
const walStatsSQL = sageTag + `SELECT wal_bytes::bigint,
  GREATEST(COALESCE(stats_reset, '-infinity'::timestamptz), pg_postmaster_start_time())
  FROM pg_stat_wal`

// readWALStats sets the WAL counter and its epoch (advisors-05). Like the
// WAL position it never fails the system stats: an unreadable counter
// stays zero, an unknown WAL volume for this cycle.
func (c *Collector) readWALStats(ctx context.Context, s *SystemStats) {
	var bytes int64
	var epoch time.Time
	if err := c.catalogQueryRow(ctx, walStatsSQL).Scan(&bytes, &epoch); err != nil {
		c.logFn("WARN", "collector: pg_stat_wal unavailable this cycle "+
			"(the WAL advisor falls back to the WAL position): %v", err)
		return
	}
	s.WALBytes, s.WALStatsEpoch = bytes, epoch.UTC()
}

// systemStatsFallback is the other version's read to retry when a server
// lacks the probed version's view or column: pg_stat_bgwriter's columns
// when pg_stat_checkpointer is missing (42P01), PG 17's when num_done is
// (42703) or when an older read meets a PG 17+ server, whose
// pg_stat_bgwriter has no checkpoints_timed (42703). "" when there is
// nothing to retry.
func systemStatsFallback(versionNum int, err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	switch {
	case pgErr.Code == "42P01" && versionNum >= 170000:
		return systemStatsSQL14
	case pgErr.Code == "42703" && versionNum >= 180000:
		return systemStatsSQL17
	case pgErr.Code == "42703" && versionNum < 170000:
		return systemStatsSQL17
	}
	return ""
}

// scanSystemStats reads one system stats row. A NULL cache hit ratio (no
// block accesses yet) becomes CacheHitRatioUnknown, never 0 (G1-B08).
func (c *Collector) scanSystemStats(ctx context.Context, sql string) (SystemStats, error) {
	var s SystemStats
	var ratio *float64
	var ckptEpoch *time.Time
	err := c.catalogQueryRow(ctx, sql).Scan(
		&s.ActiveBackends, &s.IdleInTransaction,
		&s.TotalBackends, &s.MaxConnections,
		&ratio, &s.Deadlocks,
		&s.BlkReadTime, &s.BlkWriteTime,
		&s.BlksHit, &s.BlksRead, &s.XactCommit, &s.XactRollback, &s.TempBytes, &s.Sessions,
		&s.IdleInTxMaxSeconds,
		&s.TotalCheckpoints, &s.CheckpointsTimed, &s.CheckpointsRequested,
		&s.CheckpointsDone, &ckptEpoch, &s.ReservedConnections, &s.IsReplica,
		&s.ClientBackends,
	)
	if ckptEpoch != nil {
		s.CheckpointStatsEpoch = ckptEpoch.UTC()
	}
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
