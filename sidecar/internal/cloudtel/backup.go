package cloudtel

// BackupPosture is what the provider's instance description says about
// recovery, read with the instance on every collection (no extra API
// call). Agent posture (AP-11) reports from it. A nil field is unknown,
// never "off": a response without it proves nothing.
type BackupPosture struct {
	BackupsEnabled     *bool `json:"backups_enabled,omitempty"`
	PITREnabled        *bool `json:"pitr_enabled,omitempty"`
	RetentionDays      *int  `json:"retention_days,omitempty"`
	DeletionProtection *bool `json:"deletion_protection,omitempty"`
}

func (b *BackupPosture) clone() *BackupPosture {
	if b == nil {
		return nil
	}
	return &BackupPosture{BackupsEnabled: clonePtr(b.BackupsEnabled),
		PITREnabled: clonePtr(b.PITREnabled), RetentionDays: clonePtr(b.RetentionDays),
		DeletionProtection: clonePtr(b.DeletionProtection)}
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
// evidence there.
func rdsBackup(retention *int32, deletionProtection *bool, aurora bool) *BackupPosture {
	b := &BackupPosture{}
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
