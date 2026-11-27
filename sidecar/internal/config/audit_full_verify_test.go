package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadAuditYAML(t *testing.T, raw string) *Config {
	t.Helper()
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "sage.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := loadYAML(path, cfg); err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// Without any config file, a full pass runs weekly (168 h); an audit
// block that does not name the key keeps that default.
func TestFullVerifyIntervalDefault(t *testing.T) {
	if got := DefaultConfig().Audit.FullVerifyIntervalHours; got != 168 {
		t.Fatalf("default full_verify_interval_hours = %d, want 168", got)
	}
	cfg := loadAuditYAML(t, "audit:\n  verify_interval_hours: 6\n")
	if cfg.Audit.FullVerifyIntervalHours != 168 || cfg.Audit.VerifyIntervalHours != 6 {
		t.Fatalf("partial audit block: full=%d verify=%d, want 168 and 6",
			cfg.Audit.FullVerifyIntervalHours, cfg.Audit.VerifyIntervalHours)
	}
}

// 0 is a real value (always verify in full), not "use the default".
func TestFullVerifyIntervalZeroIsKept(t *testing.T) {
	cfg := loadAuditYAML(t, "audit:\n  full_verify_interval_hours: 0\n")
	if cfg.Audit.FullVerifyIntervalHours != 0 {
		t.Fatalf("explicit 0 loaded as %d", cfg.Audit.FullVerifyIntervalHours)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("explicit 0 refused: %v", err)
	}
}

// Bounds: 0-8760 (a year); outside is refused with the key named.
func TestFullVerifyIntervalBounds(t *testing.T) {
	for _, v := range []int{-1, 8761} {
		a := defaultAuditConfig()
		a.FullVerifyIntervalHours = v
		err := a.validate()
		if err == nil || !strings.Contains(err.Error(), "audit.full_verify_interval_hours") {
			t.Fatalf("full_verify_interval_hours=%d: err = %v", v, err)
		}
	}
	for _, v := range []int{0, 1, 8760} {
		a := defaultAuditConfig()
		a.FullVerifyIntervalHours = v
		if err := a.validate(); err != nil {
			t.Fatalf("full_verify_interval_hours=%d refused: %v", v, err)
		}
	}
}
