package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D2: trust.maintenance_window uses policy.ParseWindow. Invalid values are
// rejected at load with an actionable error instead of silently meaning
// "never".

func loadTrustWindow(t *testing.T, window string) (*Config, error) {
	t.Helper()
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "trust:\n  maintenance_window: \"" + window + "\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load([]string{"--config", path})
}

func TestLoadAcceptsUnifiedWindowGrammar(t *testing.T) {
	for _, window := range []string{
		"weeknights", "never", "off", "none", "disabled", "always",
		"0 2 * * *", "0 2 * * 1-5", "Mon-Fri 01:00-05:00",
		"weekdays 01:00-05:00 America/Chicago", "0 2 * * * @30m",
	} {
		t.Run(window, func(t *testing.T) {
			cfg, err := loadTrustWindow(t, window)
			if err != nil {
				t.Fatalf("Load(maintenance_window=%q): %v", window, err)
			}
			if cfg.Trust.MaintenanceWindow != window {
				t.Fatalf("MaintenanceWindow = %q, want %q", cfg.Trust.MaintenanceWindow, window)
			}
		})
	}
}

func TestLoadRejectsInvalidMaintenanceWindow(t *testing.T) {
	for _, window := range []string{
		"weeknigths", "00:00-00:00", "0 2 * *", "weekdays 01:00-05:00 Mars/Olympus",
	} {
		t.Run(window, func(t *testing.T) {
			_, err := loadTrustWindow(t, window)
			if err == nil {
				t.Fatalf("Load(maintenance_window=%q) = nil error, want rejection", window)
			}
			if !strings.Contains(err.Error(), "trust.maintenance_window") {
				t.Fatalf("error %q must name trust.maintenance_window", err)
			}
		})
	}
}

func TestValidateMaintenanceWindow(t *testing.T) {
	for _, ok := range []string{"", "  ", "never", "NEVER", "off", "weeknights", "nights"} {
		if err := ValidateMaintenanceWindow(ok); err != nil {
			t.Errorf("ValidateMaintenanceWindow(%q) = %v, want nil", ok, err)
		}
	}
	err := ValidateMaintenanceWindow("weeknigths")
	if err == nil {
		t.Fatal("ValidateMaintenanceWindow(typo) = nil, want error")
	}
	for _, want := range []string{"trust.maintenance_window", "weeknigths", "never"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestMaintenanceWindowDisabled(t *testing.T) {
	for value, want := range map[string]bool{
		"never": true, "Off": true, " none ": true, "disabled": true,
		"": false, "always": false, "nights": false,
	} {
		if got := MaintenanceWindowDisabled(value); got != want {
			t.Errorf("MaintenanceWindowDisabled(%q) = %v, want %v", value, got, want)
		}
	}
}

// Default value masking: with no config file the window is unset (not a
// zero-width or "always" window), the lock ceiling's partner safety value
// is the documented 30000ms, and validation passes.
func TestDefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_DATABASE_URL", "")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if cfg.Trust.MaintenanceWindow != "" {
		t.Fatalf("default maintenance_window = %q, want unset", cfg.Trust.MaintenanceWindow)
	}
	if cfg.Safety.LockTimeoutMs != 30000 || cfg.Safety.LockTimeout() != 30000 {
		t.Fatalf("default lock_timeout_ms = %d (effective %d), want 30000",
			cfg.Safety.LockTimeoutMs, cfg.Safety.LockTimeout())
	}
	if cfg.Policy.Profile != "unattended" {
		t.Fatalf("default policy profile = %q, want unattended", cfg.Policy.Profile)
	}
}

// tuner.analyze_maintenance_threshold_mb never had a consumer. It is
// removed; a config that still sets it gets an actionable error.
func TestRetiredAnalyzeMaintenanceThresholdIsRejected(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "tuner:\n  analyze_maintenance_threshold_mb: 1024\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "tuner.analyze_maintenance_threshold_mb") ||
		!strings.Contains(err.Error(), "remove") {
		t.Fatalf("Load = %v, want an error naming the retired key and telling to remove it", err)
	}
}
