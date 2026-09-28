package config

import (
	"bytes"
	"io/fs"
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
// removed from the struct; a config that still sets it loads, the value is
// ignored, and exactly one actionable warning is written.
func TestRetiredAnalyzeMaintenanceThresholdIsIgnoredWithWarning(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "tuner:\n  analyze_maintenance_threshold_mb: 1024\n" +
		"  analyze_cooldown_minutes: 45\ntrust:\n  maintenance_window: nights\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })

	cfg, err := Load([]string{"--config", path})

	if err != nil {
		t.Fatalf("Load with retired key: %v", err)
	}
	if cfg.Tuner.AnalyzeCooldownMinutes != 45 || cfg.Trust.MaintenanceWindow != "nights" {
		t.Fatalf("sibling keys lost: cooldown=%d window=%q",
			cfg.Tuner.AnalyzeCooldownMinutes, cfg.Trust.MaintenanceWindow)
	}
	const warning = "tuner.analyze_maintenance_threshold_mb is no longer used and is " +
		"ignored; remove it"
	if got := strings.Count(out.String(), warning); got != 1 {
		t.Fatalf("warning count = %d in %q, want exactly 1", got, out.String())
	}
}

// A config without the retired key loads silently.
func TestConfigWithoutRetiredKeyDoesNotWarn(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("tuner:\n  analyze_cooldown_minutes: 45\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })

	if _, err := Load([]string{"--config", path}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(out.String(), "analyze_maintenance_threshold_mb") {
		t.Fatalf("unexpected warning %q", out.String())
	}
}

// Shipped example configs, fixtures and user docs must not carry the key.
func TestShippedExamplesOmitRetiredTunerKey(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "reviews", "dist", "research", "specs":
				return filepath.SkipDir
			}
			return nil
		}
		if !shippedConfigOrDoc(path) {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(body), "analyze_maintenance_threshold_mb") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("shipped files still set the retired key: %v", offenders)
	}
}

// shippedConfigOrDoc selects YAML configs and user-facing docs. Historical
// plans (docs/plan_*) and the CHANGELOG describe the past and may name it.
func shippedConfigOrDoc(path string) bool {
	name := filepath.Base(path)
	switch filepath.Ext(name) {
	case ".yaml", ".yml":
		return true
	case ".md":
		return filepath.Base(filepath.Dir(path)) == "docs" &&
			!strings.HasPrefix(name, "plan_")
	}
	return false
}
