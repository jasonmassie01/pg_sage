package collector

import "time"

// SystemInterval is what the system counters did between the previous
// snapshot and this one (collection-06): the lifetime totals of
// pg_stat_database and the checkpoint view hide a current problem behind
// months of history. A nil field is unknown: the statistics were reset in
// the interval (the epoch moved, or a counter fell under the same epoch),
// or an epoch could not be read. A value is never negative.
type SystemInterval struct {
	// Seconds is the time between the two snapshots.
	Seconds float64 `json:"seconds"`
	// CacheHitRatio is blks_hit / (blks_hit + blks_read) of the interval,
	// a fraction in [0,1]; nil also when no block was accessed.
	CacheHitRatio *float64 `json:"cache_hit_ratio"`
	BlksAccessed  *int64   `json:"blks_accessed"`
	Deadlocks     *int64   `json:"deadlocks"`
	Xacts         *int64   `json:"xacts"`
	TempBytes     *int64   `json:"temp_bytes"`
	// Sessions is how many sessions this database opened in the interval,
	// those already closed included (rca-03).
	Sessions *int64 `json:"sessions,omitempty"`
	// WALBytes is how far the cluster's WAL insert position advanced
	// between the two reads of it, WALSeconds apart (rca-03). It needs no
	// statistics epoch: nil on a standby, when either position is unread
	// and when the position went back (a failover to a server behind).
	WALBytes       *int64   `json:"wal_bytes,omitempty"`
	WALSeconds     float64  `json:"wal_seconds,omitempty"`
	BlkReadTimeMs  *float64 `json:"blk_read_time_ms"`
	BlkWriteTimeMs *float64 `json:"blk_write_time_ms"`
	// Checkpoints is cluster-wide, under its own epoch.
	Checkpoints *int64 `json:"checkpoints"`
}

// markSystemInterval sets snap's system interval against the latest
// published snapshot, read under c.mu (the API reads it concurrently).
func (c *Collector) markSystemInterval(snap *Snapshot) {
	c.mu.RLock()
	prev := c.latest
	c.mu.RUnlock()
	snap.System.Interval = computeSystemInterval(prev, snap)
}

// computeSystemInterval differences cur's system counters against prev's.
// It is nil without a previous snapshot or when cur is not after it.
// Database fields need the same known RelationStatsEpoch and no counter
// decrease; Checkpoints the same known CheckpointStatsEpoch and no
// decrease.
func computeSystemInterval(prev, cur *Snapshot) *SystemInterval {
	if prev == nil || cur == nil || !cur.CollectedAt.After(prev.CollectedAt) {
		return nil
	}
	iv := &SystemInterval{Seconds: cur.CollectedAt.Sub(prev.CollectedAt).Seconds()}
	p, s := &prev.System, &cur.System
	if sameEpoch(p.RelationStatsEpoch, s.RelationStatsEpoch) {
		databaseInterval(iv, p, s)
	}
	walInterval(iv, p, s)
	if sameEpoch(p.CheckpointStatsEpoch, s.CheckpointStatsEpoch) &&
		s.TotalCheckpoints >= p.TotalCheckpoints {
		iv.Checkpoints = int64Ptr(s.TotalCheckpoints - p.TotalCheckpoints)
	}
	return iv
}

// walInterval sets the WAL advance between two primary reads of the
// insert position (rca-03).
func walInterval(iv *SystemInterval, p, s *SystemStats) {
	if p.IsReplica || s.IsReplica || p.WALReadAt.IsZero() || s.WALReadAt.IsZero() ||
		s.WALLSN < p.WALLSN {
		return
	}
	elapsed := s.WALReadAt.Sub(p.WALReadAt).Seconds()
	if elapsed <= 0 {
		return
	}
	iv.WALBytes, iv.WALSeconds = int64Ptr(s.WALLSN-p.WALLSN), elapsed
}

// databaseInterval fills the per-database fields when no counter fell:
// a decrease under the same epoch is a reset the epoch did not record.
func databaseInterval(iv *SystemInterval, p, s *SystemStats) {
	if s.BlksHit < p.BlksHit || s.BlksRead < p.BlksRead ||
		s.Deadlocks < p.Deadlocks || s.XactCommit < p.XactCommit ||
		s.XactRollback < p.XactRollback || s.TempBytes < p.TempBytes ||
		s.BlkReadTime < p.BlkReadTime || s.BlkWriteTime < p.BlkWriteTime ||
		s.Sessions < p.Sessions {
		return
	}
	// A previous count of 0 is a row from before sessions was read: the
	// collector's own connection is always a session.
	if p.Sessions > 0 {
		iv.Sessions = int64Ptr(s.Sessions - p.Sessions)
	}
	hits := s.BlksHit - p.BlksHit
	accessed := hits + s.BlksRead - p.BlksRead
	iv.BlksAccessed = int64Ptr(accessed)
	if accessed > 0 {
		ratio := float64(hits) / float64(accessed)
		iv.CacheHitRatio = &ratio
	}
	iv.Deadlocks = int64Ptr(s.Deadlocks - p.Deadlocks)
	iv.Xacts = int64Ptr(s.XactCommit - p.XactCommit + s.XactRollback - p.XactRollback)
	iv.TempBytes = int64Ptr(s.TempBytes - p.TempBytes)
	read := s.BlkReadTime - p.BlkReadTime
	write := s.BlkWriteTime - p.BlkWriteTime
	iv.BlkReadTimeMs, iv.BlkWriteTimeMs = &read, &write
}

// sameEpoch reports two known, equal statistics epochs. An unknown (zero)
// epoch on either side cannot prove the counters are comparable.
func sameEpoch(previous, current time.Time) bool {
	return !previous.IsZero() && !current.IsZero() && previous.Equal(current)
}

func int64Ptr(v int64) *int64 { return &v }
