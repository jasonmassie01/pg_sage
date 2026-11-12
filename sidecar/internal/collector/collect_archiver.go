package collector

import (
	"context"
	"time"
)

// ArchiverStats is the server's one pg_stat_archiver row with the WAL
// position the archiver rules compare it to (market-07). Archiving is
// cluster-wide: every database of the server reads the same row.
type ArchiverStats struct {
	ArchiveMode      string     `json:"archive_mode"`
	ArchivedCount    int64      `json:"archived_count"`
	FailedCount      int64      `json:"failed_count"`
	LastArchivedWAL  *string    `json:"last_archived_wal,omitempty"`
	LastArchivedTime *time.Time `json:"last_archived_time,omitempty"`
	LastFailedTime   *time.Time `json:"last_failed_time,omitempty"`
	StatsReset       *time.Time `json:"stats_reset,omitempty"`
	// ArchivedAgeSeconds is the server's now() - last_archived_time, so the
	// gap is measured on one clock; nil when nothing was archived.
	ArchivedAgeSeconds *float64 `json:"archived_age_s,omitempty"`
	// CurrentWAL is the WAL file being written; nil on a standby.
	CurrentWAL *string `json:"current_wal,omitempty"`
	// SegmentBytes is wal_segment_size.
	SegmentBytes int64 `json:"wal_segment_bytes"`
}

// archiverSQL reads the archiver row (the columns of the SRE archiver
// probe) with the current WAL file and the segment size. It reads only
// statistics views and settings.
const archiverSQL = `/* pg_sage collector:archiver v1 */
SELECT pg_catalog.current_setting('archive_mode') AS archive_mode,
       a.archived_count::int8 AS archived_count,
       a.failed_count::int8 AS failed_count,
       a.last_archived_wal, a.last_archived_time, a.last_failed_time, a.stats_reset,
       EXTRACT(EPOCH FROM pg_catalog.now() - a.last_archived_time)::float8 AS archived_age_s,
       CASE WHEN pg_catalog.pg_is_in_recovery() THEN NULL
            ELSE pg_catalog.pg_walfile_name(pg_catalog.pg_current_wal_lsn()) END AS current_wal,
       (SELECT s.setting::int8 FROM pg_catalog.pg_settings s
         WHERE s.name = 'wal_segment_size') AS wal_segment_bytes
FROM pg_catalog.pg_stat_archiver a
LIMIT 1`

// collectArchiver reads the archiver row; a server without one (none
// exists on supported versions) is an error, not an empty archiver.
func (c *Collector) collectArchiver(ctx context.Context) (*ArchiverStats, error) {
	var a ArchiverStats
	var segment *int64
	err := c.catalogQueryRow(ctx, archiverSQL).Scan(&a.ArchiveMode, &a.ArchivedCount,
		&a.FailedCount, &a.LastArchivedWAL, &a.LastArchivedTime, &a.LastFailedTime,
		&a.StatsReset, &a.ArchivedAgeSeconds, &a.CurrentWAL, &segment)
	if err != nil {
		return nil, err
	}
	if segment != nil {
		a.SegmentBytes = *segment
	}
	return &a, nil
}
