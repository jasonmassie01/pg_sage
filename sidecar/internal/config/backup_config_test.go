package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// market-07: backup.max_age_hours (26), backup.max_archive_gap_minutes (15)
// and backup.min_retention_days (7) are the backup freshness policy.

// No concurrent access tests: BackupConfig is a value read after load.

func TestBackupDefaultsWithoutAConfigFile(t *testing.T) {
	c := DefaultConfig()
	b := c.Backup
	if b.MaxAgeHours != 26 || b.MaxArchiveGapMinutes != 15 || b.MinRetentionDays != 7 {
		t.Fatalf("defaults = %+v, want 26 h, 15 min, 7 days", b)
	}
	if b.MaxAge() != 26*time.Hour || b.MaxArchiveGap() != 15*time.Minute {
		t.Fatalf("durations = %v, %v", b.MaxAge(), b.MaxArchiveGap())
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the default config is invalid: %v", err)
	}
}

// A zero-value section (a config built without DefaultConfig) keeps the
// default thresholds, never "every backup is stale" or "no gap allowed".
func TestBackupZeroValueSectionUsesTheDefaults(t *testing.T) {
	var b BackupConfig
	if b.MaxAge() != 26*time.Hour || b.MaxArchiveGap() != 15*time.Minute {
		t.Fatalf("zero-value durations = %v, %v, want 26h, 15m", b.MaxAge(), b.MaxArchiveGap())
	}
}

func TestBackupLoadsFromYAML(t *testing.T) {
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "sage.yaml")
	body := "backup:\n  max_age_hours: 50\n  max_archive_gap_minutes: 30\n" +
		"  min_retention_days: 14\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Backup
	if b.MaxAgeHours != 50 || b.MaxArchiveGapMinutes != 30 || b.MinRetentionDays != 14 {
		t.Fatalf("loaded = %+v", b)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("loaded config invalid: %v", err)
	}
}

// Invalid input and boundaries: a negative retention is refused at load,
// 0 turns the retention check off, and each threshold has a range.
func TestBackupValidationBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		set     func(*BackupConfig)
		wantErr string
	}{
		{"min_retention_days=-1", func(b *BackupConfig) { b.MinRetentionDays = -1 },
			"backup.min_retention_days"},
		{"min_retention_days=0 (off)", func(b *BackupConfig) { b.MinRetentionDays = 0 }, ""},
		{"min_retention_days=35", func(b *BackupConfig) { b.MinRetentionDays = 35 }, ""},
		{"min_retention_days=36", func(b *BackupConfig) { b.MinRetentionDays = 36 },
			"backup.min_retention_days"},
		{"max_age_hours=0", func(b *BackupConfig) { b.MaxAgeHours = 0 },
			"backup.max_age_hours"},
		{"max_age_hours=1", func(b *BackupConfig) { b.MaxAgeHours = 1 }, ""},
		{"max_age_hours=-5", func(b *BackupConfig) { b.MaxAgeHours = -5 },
			"backup.max_age_hours"},
		{"max_age_hours=8760", func(b *BackupConfig) { b.MaxAgeHours = 8760 }, ""},
		{"max_age_hours=8761", func(b *BackupConfig) { b.MaxAgeHours = 8761 },
			"backup.max_age_hours"},
		{"max_archive_gap_minutes=0", func(b *BackupConfig) { b.MaxArchiveGapMinutes = 0 },
			"backup.max_archive_gap_minutes"},
		{"max_archive_gap_minutes=1", func(b *BackupConfig) { b.MaxArchiveGapMinutes = 1 }, ""},
		{"max_archive_gap_minutes=10081",
			func(b *BackupConfig) { b.MaxArchiveGapMinutes = 10081 },
			"backup.max_archive_gap_minutes"},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		tc.set(&c.Backup)
		err := c.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s rejected: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want one naming %s", tc.name, err, tc.wantErr)
		}
	}
}

// Error propagation: a YAML file with a negative retention fails the
// whole load with the key named, so a typo cannot silently disable it.
func TestBackupNegativeRetentionRefusedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sage.yaml")
	body := "postgres:\n  host: localhost\nbackup:\n  min_retention_days: -1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := DefaultConfig()
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "backup.min_retention_days") ||
		!strings.Contains(err.Error(), "-1") {
		t.Fatalf("validate = %v, want a refusal naming backup.min_retention_days and -1", err)
	}
}
