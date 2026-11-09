package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: config loading is single-threaded and the
// retired-key table is read-only.

const managedServiceWarning = "migration.managed_service is no longer used and is ignored"

// loadWithWarnings is shared with ignored_keys_test.go.

// Nil/empty (migration-12): migration.managed_service never filtered
// anything. A strict-YAML config that still sets it loads, its sibling
// migration keys survive, and exactly one warning names the key.
func TestRetiredManagedServiceLoadsWithOneWarning(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "migration:\n  enabled: true\n"+
		"  managed_service: rds\n  ddl_row_threshold: 777\n")
	if err != nil {
		t.Fatalf("Load with managed_service: %v", err)
	}
	if !cfg.Migration.Enabled || cfg.Migration.DDLRowThreshold != 777 {
		t.Fatalf("sibling keys lost: enabled=%v ddl_row_threshold=%d",
			cfg.Migration.Enabled, cfg.Migration.DDLRowThreshold)
	}
	if got := strings.Count(out, managedServiceWarning); got != 1 {
		t.Fatalf("warning count = %d in %q, want exactly 1", got, out)
	}
	if !strings.Contains(out, "pg_repack") {
		t.Fatalf("warning %q does not say what replaced the key", out)
	}
}

// Boundary: a migration section holding only the retired key loads with
// the shipped migration defaults.
func TestRetiredManagedServiceAloneKeepsDefaults(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "migration:\n  managed_service: cloudsql\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := DefaultConfig().Migration
	if cfg.Migration != want {
		t.Fatalf("migration = %+v, want defaults %+v", cfg.Migration, want)
	}
	if strings.Count(out, managedServiceWarning) != 1 {
		t.Fatalf("warnings %q, want one managed_service warning", out)
	}
}

// A config without the key does not warn about it.
func TestConfigWithoutManagedServiceDoesNotWarn(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "migration:\n  enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Migration.Enabled {
		t.Fatal("migration.enabled lost")
	}
	if strings.Contains(out, "managed_service") {
		t.Fatalf("unexpected warning %q", out)
	}
}

// Both retired-key sections in one file: each warns once and both
// sections keep their live keys.
func TestRetiredKeysAcrossSections(t *testing.T) {
	cfg, out, err := loadWithWarnings(t, "tuner:\n  analyze_maintenance_threshold_mb: 9\n"+
		"  analyze_cooldown_minutes: 45\nmigration:\n  managed_service: rds\n"+
		"  poll_interval_seconds: 9\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tuner.AnalyzeCooldownMinutes != 45 || cfg.Migration.PollIntervalSeconds != 9 {
		t.Fatalf("live keys lost: cooldown=%d poll=%d",
			cfg.Tuner.AnalyzeCooldownMinutes, cfg.Migration.PollIntervalSeconds)
	}
	if strings.Count(out, managedServiceWarning) != 1 ||
		strings.Count(out, "analyze_maintenance_threshold_mb") != 1 {
		t.Fatalf("warnings %q, want one per retired key", out)
	}
}

// Invalid input: an unknown migration key is still rejected (only the
// retired key is forgiven).
func TestUnknownMigrationKeyStillRejected(t *testing.T) {
	_, _, err := loadWithWarnings(t, "migration:\n  managed_servise: rds\n")
	if err == nil || !strings.Contains(err.Error(), "managed_servise") {
		t.Fatalf("err = %v, want a strict-decode error naming the typo", err)
	}
}
