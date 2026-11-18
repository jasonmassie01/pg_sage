package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// drill-01b: backup.min_pitr_hours (24, 0 turns the check off) grades the
// provider's point-in-time window; backup.external_source names a backup
// kept outside the provider's delete path.

// No concurrent access tests: BackupConfig is a value read after load.

func TestBackupIsolationDefaultsWithoutAConfigFile(t *testing.T) {
	b := DefaultConfig().Backup
	if b.MinPITRHours != 24 || b.ExternalSource != "" {
		t.Fatalf("defaults = %+v, want min_pitr_hours 24 and no external source", b)
	}
	if b.MinPITRWindow() != 24*time.Hour {
		t.Fatalf("MinPITRWindow = %v, want 24h", b.MinPITRWindow())
	}
}

// A zero-value section keeps the check off rather than inventing a policy
// the operator never loaded; DefaultConfig is what sets 24.
func TestBackupIsolationZeroValueIsOff(t *testing.T) {
	var b BackupConfig
	if b.MinPITRWindow() != 0 {
		t.Fatalf("zero-value MinPITRWindow = %v, want 0 (off)", b.MinPITRWindow())
	}
}

func TestBackupIsolationLoadsFromYAML(t *testing.T) {
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "sage.yaml")
	body := "backup:\n  min_pitr_hours: 72\n" +
		"  external_source: \"AWS Backup vault orders-vault in account 2\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Backup
	if b.MinPITRHours != 72 || b.ExternalSource != "AWS Backup vault orders-vault in account 2" {
		t.Fatalf("loaded = %+v", b)
	}
	if b.MinPITRWindow() != 72*time.Hour {
		t.Fatalf("MinPITRWindow = %v, want 72h", b.MinPITRWindow())
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("loaded config invalid: %v", err)
	}
}

// Invalid input and boundaries.
func TestBackupIsolationValidationBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		set     func(*BackupConfig)
		wantErr string
	}{
		{"min_pitr_hours=-1", func(b *BackupConfig) { b.MinPITRHours = -1 },
			"backup.min_pitr_hours"},
		{"min_pitr_hours=0 (off)", func(b *BackupConfig) { b.MinPITRHours = 0 }, ""},
		{"min_pitr_hours=8760", func(b *BackupConfig) { b.MinPITRHours = 8760 }, ""},
		{"min_pitr_hours=8761", func(b *BackupConfig) { b.MinPITRHours = 8761 },
			"backup.min_pitr_hours"},
		{"external_source 200 chars", func(b *BackupConfig) {
			b.ExternalSource = strings.Repeat("v", 200)
		}, ""},
		{"external_source 201 chars", func(b *BackupConfig) {
			b.ExternalSource = strings.Repeat("v", 201)
		}, "backup.external_source"},
		{"external_source with a newline", func(b *BackupConfig) {
			b.ExternalSource = "vault\nforged log line"
		}, "backup.external_source"},
		{"external_source blank", func(b *BackupConfig) { b.ExternalSource = "   " },
			"backup.external_source"},
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
