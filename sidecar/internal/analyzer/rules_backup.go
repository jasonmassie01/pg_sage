package analyzer

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// The WAL archiver rules (market-07) report backup results, not backup
// configuration: AP-11 owns "archive_mode is off". Archiving is
// cluster-wide, so in fleet mode only the instance's owner reports it.

const (
	categoryWALArchiveFailing = "wal_archive_failing"
	categoryWALArchiveStale   = "wal_archive_stale"
	archiverIdentity          = "archiver"
	// archiveFailCritical is the failure streak that makes a failing
	// archiver critical.
	archiveFailCritical = 30 * time.Minute
	// archiveStaleCritical is the multiple of the gap that makes a stale
	// archiver critical.
	archiveStaleCritical = 4
)

// WithInstanceOwner tells the analyzer which database of the fleet owns
// this database's instance (advisor.InstanceOwner's shape): a database
// that is not the owner leaves the cluster-wide archiver findings to it.
// Nil (the default) reports them here, as in single-database mode.
func (a *Analyzer) WithInstanceOwner(fn func() (owner string, isOwner bool)) {
	a.instanceOwner = fn
}

// loadArchiverOwner records, once per cycle, the database the archiver
// findings are left to ("" when this database reports them).
func (a *Analyzer) loadArchiverOwner() {
	a.extras.ArchiverOwner = ""
	if a.instanceOwner == nil {
		return
	}
	if owner, isOwner := a.instanceOwner(); !isOwner && owner != "" {
		a.extras.ArchiverOwner = owner
	}
}

func archiverDeferred(extras *RuleExtras) bool {
	return extras != nil && extras.ArchiverOwner != ""
}

func hasArchiver(cur, _ *collector.Snapshot) bool {
	return cur.Archiver != nil && cur.Available("archiver")
}

// hasArchiverInterval needs both snapshots' archiver rows from one
// statistics epoch: failed_count only compares within one.
func hasArchiverInterval(cur, prev *collector.Snapshot) bool {
	if !hasArchiver(cur, nil) || prev == nil || !hasArchiver(prev, nil) {
		return false
	}
	c, p := cur.Archiver.StatsReset, prev.Archiver.StatsReset
	return (c == nil) == (p == nil) && (c == nil || c.Equal(*p))
}

func archivingOn(a *collector.ArchiverStats) bool {
	return a != nil && (a.ArchiveMode == "on" || a.ArchiveMode == "always")
}

// archiverFailing: the last failure is after the last success.
func archiverFailing(a *collector.ArchiverStats) bool {
	return a.LastFailedTime != nil &&
		(a.LastArchivedTime == nil || a.LastFailedTime.After(*a.LastArchivedTime))
}

// archiverStillFailing reports an archiver that has not archived since
// it last failed: an open wal_archive_failing finding is kept although
// failed_count did not move between the archiver's retries.
func archiverStillFailing(cur *collector.Snapshot) bool {
	return hasArchiver(cur, nil) && archivingOn(cur.Archiver) && archiverFailing(cur.Archiver)
}

// failureStreak is how long the archiver has failed without a success,
// on the server's clock: from the last success (or the statistics reset)
// to the last failure.
func failureStreak(a *collector.ArchiverStats) (time.Duration, bool) {
	start := a.LastArchivedTime
	if start == nil {
		start = a.StatsReset
	}
	if start == nil {
		return 0, false
	}
	return a.LastFailedTime.Sub(*start), true
}

// ruleWALArchiveFailing fires when failed_count rose since the previous
// snapshot and the last failure is after the last success. The analyzer
// runs a rule even when its Needs fails, so the input is checked here.
func ruleWALArchiveFailing(cur, prev *collector.Snapshot, _ *config.Config,
	extras *RuleExtras) []Finding {
	if archiverDeferred(extras) || !hasArchiverInterval(cur, prev) {
		return nil
	}
	a, p := cur.Archiver, prev.Archiver
	if !archivingOn(a) || !archiverFailing(a) || a.FailedCount <= p.FailedCount {
		return nil
	}
	streak, known := failureStreak(a)
	severity := "warning"
	if known && streak >= archiveFailCritical {
		severity = "critical"
	}
	title := "WAL archiving is failing: archive_command has not succeeded since its " +
		"last failure"
	if known {
		title = fmt.Sprintf("WAL archiving has failed for %s without a successful archive",
			streak.Round(time.Minute))
	}
	detail := archiverDetail(a)
	detail["failed_since_last_cycle"] = a.FailedCount - p.FailedCount
	detail["streak_known"] = known
	if known {
		detail["streak_minutes"] = streak.Minutes()
	}
	return []Finding{archiverFinding(categoryWALArchiveFailing, severity, title, detail,
		"Point-in-time recovery stops at the last archived WAL and pg_wal grows until "+
			"archiving recovers. Read the server log for the archive_command or "+
			"archive_library error (destination full, credentials, network) and fix "+
			"it; PostgreSQL retries the waiting segments itself.")}
}

// ruleWALArchiveStale fires when archiving is on, completed WAL segments
// wait and the last successful archive is older than the configured gap.
// A failing archiver is wal_archive_failing's.
func ruleWALArchiveStale(cur, _ *collector.Snapshot, cfg *config.Config,
	extras *RuleExtras) []Finding {
	if archiverDeferred(extras) || !hasArchiver(cur, nil) {
		return nil
	}
	a := cur.Archiver
	if !archivingOn(a) || archiverFailing(a) || a.ArchivedAgeSeconds == nil {
		return nil
	}
	pending, known := pendingSegments(a)
	gap := cfg.Backup.MaxArchiveGap()
	age := time.Duration(*a.ArchivedAgeSeconds * float64(time.Second))
	if !known || pending < 1 || age <= gap {
		return nil
	}
	severity := "warning"
	if age >= archiveStaleCritical*gap {
		severity = "critical"
	}
	detail := archiverDetail(a)
	detail["pending_segments"] = pending
	detail["archived_age_minutes"] = age.Minutes()
	detail["max_archive_gap_minutes"] = cfg.Backup.MaxArchiveGapMinutes
	title := fmt.Sprintf("No WAL archived for %s while %d completed segments wait",
		age.Round(time.Minute), pending)
	return []Finding{archiverFinding(categoryWALArchiveStale, severity, title, detail,
		"The archiver is not keeping up or is stuck without reporting failures: check "+
			"that archive_command returns (a hung copy blocks every later segment) and "+
			"the archive destination's throughput. Raise backup.max_archive_gap_minutes "+
			"only if a slower archive is intended.")}
}

func archiverFinding(category, severity, title string, detail map[string]any,
	advice string) Finding {
	return Finding{Category: category, Severity: severity, ObjectType: "archiver",
		ObjectIdentifier: archiverIdentity, Title: title, Detail: detail,
		Recommendation: advice, ActionRisk: "safe"}
}

func archiverDetail(a *collector.ArchiverStats) map[string]any {
	d := map[string]any{"archive_mode": a.ArchiveMode, "failed_count": a.FailedCount,
		"archived_count": a.ArchivedCount}
	if a.LastArchivedWAL != nil {
		d["last_archived_wal"] = *a.LastArchivedWAL
	}
	if a.LastArchivedTime != nil {
		d["last_archived_time"] = a.LastArchivedTime.UTC().Format(time.RFC3339)
	}
	if a.LastFailedTime != nil {
		d["last_failed_time"] = a.LastFailedTime.UTC().Format(time.RFC3339)
	}
	return d
}

// pendingSegments is the number of completed WAL segments after the last
// archived one: the segments between it and the one being written. It is
// unknown on a standby, without a segment size, or when the last archived
// file is not a segment (a timeline history file).
func pendingSegments(a *collector.ArchiverStats) (int64, bool) {
	if a == nil || a.LastArchivedWAL == nil || a.CurrentWAL == nil || a.SegmentBytes <= 0 {
		return 0, false
	}
	last, ok := segmentNumber(*a.LastArchivedWAL, a.SegmentBytes)
	if !ok {
		return 0, false
	}
	cur, ok := segmentNumber(*a.CurrentWAL, a.SegmentBytes)
	if !ok || cur <= last {
		return 0, ok
	}
	return cur - last - 1, true
}

// segmentNumber reads a WAL file name's segment number (timeline, log and
// segment as three 8-digit hex fields; a .backup or .partial suffix names
// the same segment). The timeline is ignored: segment numbers continue
// across a promotion.
func segmentNumber(name string, segmentBytes int64) (int64, bool) {
	if len(name) < 24 {
		return 0, false
	}
	logID, err1 := strconv.ParseUint(name[8:16], 16, 32)
	seg, err2 := strconv.ParseUint(name[16:24], 16, 32)
	if _, err := strconv.ParseUint(name[0:8], 16, 32); err != nil || err1 != nil ||
		err2 != nil {
		return 0, false
	}
	perLog := int64(0x100000000 / uint64(segmentBytes))
	if perLog <= 0 {
		return 0, false
	}
	return int64(logID)*perLog + int64(seg), true
}
