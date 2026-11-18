package config

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// BackupConfig is the backup freshness policy (market-07): how old the
// provider's last good backup may be, how long completed WAL may wait for
// the archiver, and the retention the operator requires.
type BackupConfig struct {
	MaxAgeHours          int `yaml:"max_age_hours" doc:"Hours the provider's last good backup (RDS and Aurora latest restorable time, Cloud SQL last successful backup run) may age before backup_stale opens; critical at twice the age. 1-8760. Default: 26."`
	MaxArchiveGapMinutes int `yaml:"max_archive_gap_minutes" doc:"Minutes completed WAL may wait since the last successful archive before wal_archive_stale opens; critical at four times the gap. 1-10080. Default: 15."`
	MinRetentionDays     int `yaml:"min_retention_days" doc:"Backup retention in days the provider must keep; less opens backup_retention_below_policy (0 days is AP-11's backups-off case). 0 turns the check off. 0-35. Default: 7."`
	MinPITRHours         int `yaml:"min_pitr_hours" doc:"Hours of point-in-time window the provider must offer; less opens pitr_window_below_policy. 0 turns the check off. 0-8760. Default: 24."`
	// ExternalSource names a backup kept outside the provider's delete
	// path (drill-01b), which pg_sage cannot read.
	ExternalSource string `yaml:"external_source" doc:"A backup outside the provider's delete path that pg_sage cannot read (another account's vault, an external dump). Set, it resolves backup_shares_delete_path. One line, max 200 chars. Default: empty."`
}

// Backup policy defaults.
const (
	DefaultBackupMaxAgeHours          = 26
	DefaultBackupMaxArchiveGapMinutes = 15
	DefaultBackupMinRetentionDays     = 7
	DefaultBackupMinPITRHours         = 24
)

// Backup policy bounds.
const (
	maxBackupMaxAgeHours      = 8760
	maxBackupArchiveGapMins   = 10080
	maxBackupMinRetentionDays = 35
	maxBackupMinPITRHours     = 8760
	maxBackupExternalSource   = 200
)

func defaultBackupConfig() BackupConfig {
	return BackupConfig{MaxAgeHours: DefaultBackupMaxAgeHours,
		MaxArchiveGapMinutes: DefaultBackupMaxArchiveGapMinutes,
		MinRetentionDays:     DefaultBackupMinRetentionDays,
		MinPITRHours:         DefaultBackupMinPITRHours}
}

// MaxAge is the backup age limit; an unset (zero-value) section is the
// default, never "every backup is stale".
func (b BackupConfig) MaxAge() time.Duration {
	if b.MaxAgeHours <= 0 {
		return DefaultBackupMaxAgeHours * time.Hour
	}
	return time.Duration(b.MaxAgeHours) * time.Hour
}

// MaxArchiveGap is the archive gap limit; zero is the default.
func (b BackupConfig) MaxArchiveGap() time.Duration {
	if b.MaxArchiveGapMinutes <= 0 {
		return DefaultBackupMaxArchiveGapMinutes * time.Minute
	}
	return time.Duration(b.MaxArchiveGapMinutes) * time.Minute
}

// MinPITRWindow is the point-in-time window policy; 0 (also a zero-value
// section) turns the check off.
func (b BackupConfig) MinPITRWindow() time.Duration {
	if b.MinPITRHours <= 0 {
		return 0
	}
	return time.Duration(b.MinPITRHours) * time.Hour
}

func (b BackupConfig) validate() error {
	if b.MaxAgeHours < 1 || b.MaxAgeHours > maxBackupMaxAgeHours {
		return fmt.Errorf("backup.max_age_hours must be 1-%d, got %d",
			maxBackupMaxAgeHours, b.MaxAgeHours)
	}
	if b.MaxArchiveGapMinutes < 1 || b.MaxArchiveGapMinutes > maxBackupArchiveGapMins {
		return fmt.Errorf("backup.max_archive_gap_minutes must be 1-%d, got %d",
			maxBackupArchiveGapMins, b.MaxArchiveGapMinutes)
	}
	if b.MinRetentionDays < 0 || b.MinRetentionDays > maxBackupMinRetentionDays {
		return fmt.Errorf("backup.min_retention_days must be 0-%d (0 turns the check off), "+
			"got %d", maxBackupMinRetentionDays, b.MinRetentionDays)
	}
	if b.MinPITRHours < 0 || b.MinPITRHours > maxBackupMinPITRHours {
		return fmt.Errorf("backup.min_pitr_hours must be 0-%d (0 turns the check off), "+
			"got %d", maxBackupMinPITRHours, b.MinPITRHours)
	}
	return validExternalSource(b.ExternalSource)
}

// validExternalSource: empty, or one printable line of at most 200
// characters (it reaches findings and logs).
func validExternalSource(s string) error {
	if s == "" {
		return nil
	}
	if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > maxBackupExternalSource ||
		strings.ContainsFunc(s, unicode.IsControl) {
		return fmt.Errorf("backup.external_source must be one line of at most %d "+
			"printable characters naming the backup kept outside the provider, or empty",
			maxBackupExternalSource)
	}
	return nil
}
