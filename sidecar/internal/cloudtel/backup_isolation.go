package cloudtel

import (
	"context"
	"fmt"
	"time"
)

// Backup isolation (drill-01b): a backup deleted in the same action that
// deletes the database is no backup against that action. RDS and Aurora
// automated backups are deleted with the instance unless the deleting
// call retains them (the DeleteDBInstance API deletes them by default);
// manual snapshots, AWS Backup recovery points and cross-region
// replications outlive it. An instance in a cluster (Aurora, or an RDS
// Multi-AZ DB cluster) keeps its snapshots and restore window on the
// cluster. The snapshot and automated-backup reads are cached 6 h (a
// failed read 15 min); a snapshot's age is judged on every collection
// against IsolationMaxAge.

const (
	isolationRefresh       = 6 * time.Hour
	isolationRetry         = 15 * time.Minute
	defaultIsolationMaxAge = 26 * time.Hour
)

// rdsRecordsRead is one read of an instance's or cluster's backup records.
type rdsRecordsRead struct {
	key       string
	at        time.Time
	snapshots []time.Time
	truncated bool           // more snapshots than one page lists
	window    *time.Duration // RDS: the active automated backup's restore window
	reason    string
}

func (r *rdsRecordsRead) fresh(key string, now time.Time) bool {
	if r == nil || r.key != key || now.Before(r.at) {
		return false
	}
	ttl := isolationRefresh
	if r.reason != "" {
		ttl = isolationRetry
	}
	return now.Sub(r.at) < ttl
}

// backupRecords returns the records as last read, reading them again when
// the cached read has expired. A read cut short by the caller's context is
// returned but not cached.
func (s *AWSSource) backupRecords(ctx context.Context, inst rdsInstance, cluster string,
	now time.Time) rdsRecordsRead {
	key := inst.ID + "|" + cluster
	s.mu.Lock()
	cached := s.records
	s.mu.Unlock()
	if cached.fresh(key, now) {
		return *cached
	}
	res := rdsRecordsRead{key: key, at: now}
	var err error
	res.snapshots, res.truncated, err = s.rds.manualSnapshots(ctx, inst.ID, cluster)
	if err == nil && cluster == "" && inst.ResourceID != "" {
		res.window, err = s.rds.restoreWindow(ctx, inst.ResourceID)
	}
	if ctx.Err() != nil {
		return rdsRecordsRead{reason: "backup records read cancelled"}
	}
	if err != nil {
		res = rdsRecordsRead{key: key, at: now, reason: sanitize(err.Error())}
	}
	s.mu.Lock()
	s.records = &res
	s.mu.Unlock()
	return res
}

// applyIsolation records whether a backup survives a delete and the
// point-in-time window on b. The instance description's own evidence
// (AWS Backup, replication) needs no record read.
func (s *AWSSource) applyIsolation(ctx context.Context, b *BackupPosture, inst rdsInstance,
	cluster string, now time.Time) {
	read := s.backupRecords(ctx, inst, cluster, now)
	clustered := cluster != ""
	if b.PITREnabled != nil && *b.PITREnabled {
		if clustered {
			b.PITRWindow = clusterWindow(inst)
		} else if read.reason == "" {
			b.PITRWindow = clonePtr(read.window)
		}
	}
	if v, why, ok := describedIsolation(inst); ok {
		b.SurvivesDelete, b.IsolationEvidence = &v, why
		return
	}
	if read.reason != "" {
		b.IsolationUnavailable = read.reason
		return
	}
	maxAge := s.opts.IsolationMaxAge
	if maxAge <= 0 {
		maxAge = defaultIsolationMaxAge
	}
	v, why := snapshotIsolation(read.snapshots, clustered, now, maxAge)
	if !v && read.truncated {
		b.IsolationUnavailable = fmt.Sprintf("more than %d manual snapshots are listed and "+
			"none of the first %d is within %.0f h; a newer one may be on a later page",
			maxBackupRecords, maxBackupRecords, maxAge.Hours())
		return
	}
	b.SurvivesDelete, b.IsolationEvidence = &v, why
}

// clusterWindow is the cluster's latest minus earliest restorable time.
func clusterWindow(inst rdsInstance) *time.Duration {
	if inst.LatestRestorable == nil || inst.EarliestRestorable == nil ||
		inst.LatestRestorable.Before(*inst.EarliestRestorable) {
		return nil
	}
	d := inst.LatestRestorable.Sub(*inst.EarliestRestorable)
	return &d
}

// describedIsolation is the evidence in the instance or cluster
// description itself.
func describedIsolation(inst rdsInstance) (bool, string, bool) {
	switch {
	case inst.AWSBackupARN != "":
		return true, "AWS Backup holds a recovery point of this database (" +
			sanitize(inst.AWSBackupARN) + "), which outlives the instance; the vault " +
			"and its freshness are not read", true
	case inst.Replications > 0:
		return true, fmt.Sprintf("automated backups are replicated to %d other "+
			"region(s), where they outlive the instance; their freshness is not read",
			inst.Replications), true
	}
	return false, "", false
}

// snapshotIsolation: a manual snapshot at most maxAge old survives a
// delete; a future one (beyond the clock-skew allowance) proves nothing.
func snapshotIsolation(snapshots []time.Time, clustered bool, now time.Time,
	maxAge time.Duration) (bool, string) {
	kind := "manual snapshot"
	if clustered {
		kind = "manual cluster snapshot"
	}
	var newest *time.Time
	for i, at := range snapshots {
		if now.Sub(at) < -MaxClockSkew {
			continue
		}
		if newest == nil || at.After(*newest) {
			newest = &snapshots[i]
		}
	}
	if newest != nil && now.Sub(*newest) <= maxAge {
		return true, fmt.Sprintf("a %s taken %s (%.1f h old) outlives the instance", kind,
			newest.UTC().Format(time.RFC3339), now.Sub(*newest).Hours())
	}
	last := "none listed"
	if newest != nil {
		last = "newest " + newest.UTC().Format(time.RFC3339)
	}
	return false, fmt.Sprintf("only automated backups, which are deleted with the "+
		"instance unless retained at delete time; no %s within %.0f h (%s); AWS Backup "+
		"vaults are not read", kind, maxAge.Hours(), last)
}
