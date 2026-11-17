package cloudtel

import "time"

// BackupPosture is what the provider's instance description says about
// recovery, read with the instance on every collection (no extra API
// call). Agent posture (AP-11) reports from it. A nil field is unknown,
// never "off": a response without it proves nothing.
//
// The freshness fields are backup results (market-07): RDS and Aurora's
// latest restorable time (in the instance or cluster description) and
// Cloud SQL's last successful backup run (one bounded backupRuns.list).
// FreshnessUnavailable says why the results could not be read; the
// configuration fields stay as read.
//
// The isolation fields (drill-01b) say whether a backup survives the
// deletion of the instance (SurvivesDelete, with the IsolationEvidence
// that decided it) and how wide the point-in-time window is. Nil is
// unknown; IsolationUnavailable says why the records could not be read.
type BackupPosture struct {
	BackupsEnabled       *bool      `json:"backups_enabled,omitempty"`
	PITREnabled          *bool      `json:"pitr_enabled,omitempty"`
	RetentionDays        *int       `json:"retention_days,omitempty"`
	DeletionProtection   *bool      `json:"deletion_protection,omitempty"`
	LatestRestorableTime *time.Time `json:"latest_restorable_time,omitempty"`
	LastSuccessfulBackup *time.Time `json:"last_successful_backup,omitempty"`
	// NoRecentSuccess: the latest backup runs listed include failures and
	// no successful run.
	NoRecentSuccess      bool   `json:"no_recent_success,omitempty"`
	FreshnessUnavailable string `json:"freshness_unavailable,omitempty"`
	SurvivesDelete       *bool  `json:"survives_delete,omitempty"`
	IsolationEvidence    string `json:"isolation_evidence,omitempty"`
	IsolationUnavailable string `json:"isolation_unavailable,omitempty"`
	// PITRWindow is how far back point-in-time recovery reaches.
	PITRWindow *time.Duration `json:"pitr_window_ns,omitempty"`
}

// Freshness sources LastGood names.
const (
	FreshnessRestorableTime = "latest_restorable_time"
	FreshnessBackupRun      = "last_successful_backup"
)

// LastGood is the provider's latest good backup point and where it came
// from; false when unknown (a nil posture, no result read).
func (b *BackupPosture) LastGood() (time.Time, string, bool) {
	switch {
	case b == nil:
		return time.Time{}, "", false
	case b.LatestRestorableTime != nil:
		return *b.LatestRestorableTime, FreshnessRestorableTime, true
	case b.LastSuccessfulBackup != nil:
		return *b.LastSuccessfulBackup, FreshnessBackupRun, true
	}
	return time.Time{}, "", false
}

func (b *BackupPosture) clone() *BackupPosture {
	if b == nil {
		return nil
	}
	c := *b
	c.BackupsEnabled, c.PITREnabled = clonePtr(b.BackupsEnabled), clonePtr(b.PITREnabled)
	c.RetentionDays = clonePtr(b.RetentionDays)
	c.DeletionProtection = clonePtr(b.DeletionProtection)
	c.LatestRestorableTime = clonePtr(b.LatestRestorableTime)
	c.LastSuccessfulBackup = clonePtr(b.LastSuccessfulBackup)
	c.SurvivesDelete, c.PITRWindow = clonePtr(b.SurvivesDelete), clonePtr(b.PITRWindow)
	return &c
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// rdsBackup reads RDS's automated-backup retention: 0 turns automated
// backups, and with them point-in-time recovery, off. Aurora keeps
// deletion protection on the cluster, so the instance's flag is not
// evidence there. restorable is the latest restorable time (the
// cluster's on Aurora).
func rdsBackup(retention *int32, deletionProtection *bool, aurora bool,
	restorable *time.Time) *BackupPosture {
	b := &BackupPosture{LatestRestorableTime: clonePtr(restorable)}
	if retention != nil {
		days := int(*retention)
		on := days > 0
		b.RetentionDays, b.BackupsEnabled, b.PITREnabled = &days, &on, &on
	}
	if deletionProtection != nil && !aurora {
		v := *deletionProtection
		b.DeletionProtection = &v
	}
	return b
}

// cloudSQLBackup is the Cloud SQL Admin API's backup settings; pointers
// keep absent fields unknown.
type cloudSQLBackup struct {
	Enabled                     *bool `json:"enabled"`
	PointInTimeRecoveryEnabled  *bool `json:"pointInTimeRecoveryEnabled"`
	TransactionLogRetentionDays *int  `json:"transactionLogRetentionDays"`
}

func (c *cloudSQLBackup) posture(deletionProtection *bool) *BackupPosture {
	b := &BackupPosture{DeletionProtection: deletionProtection}
	if c != nil {
		b.BackupsEnabled = c.Enabled
		b.PITREnabled = c.PointInTimeRecoveryEnabled
		b.RetentionDays = c.TransactionLogRetentionDays
	}
	return b.clone()
}

// cloudSQLIsolation (drill-01b): settings.retainBackupsOnDelete says
// whether the instance's backups outlive it (absent is unknown); the
// point-in-time window is the transaction log retention while PITR is on.
func cloudSQLIsolation(b *BackupPosture, retain *bool) {
	if retain != nil {
		v := *retain
		b.SurvivesDelete = &v
		b.IsolationEvidence = "Cloud SQL deletes the instance's backups with it " +
			"(settings.retainBackupsOnDelete is false); exports and final backups " +
			"are not read"
		if v {
			b.IsolationEvidence = "Cloud SQL retains the instance's backups after it " +
				"is deleted (settings.retainBackupsOnDelete)"
		}
	}
	if b.PITREnabled != nil && *b.PITREnabled && b.RetentionDays != nil &&
		*b.RetentionDays > 0 {
		w := time.Duration(*b.RetentionDays) * 24 * time.Hour
		b.PITRWindow = &w
	}
}

// Backup is a copy of the latest sample's backup posture; nil before the
// first good poll, after a failed one, or on a nil runtime.
func (r *Runtime) Backup() *BackupPosture {
	if r == nil {
		return nil
	}
	s, _ := r.current()
	if s == nil {
		return nil
	}
	return s.Backup.clone()
}
