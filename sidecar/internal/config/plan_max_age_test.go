package config

import (
	"os"
	"path/filepath"
	"testing"
)

// tuning-08: tuner.plan_max_age_minutes defaults to 1440 with no config
// file, and an explicit 0 keeps the default rather than "never".

func TestDefaultConfig_PlanMaxAgeMinutes(t *testing.T) {
	if got := DefaultConfig().Tuner.PlanMaxAgeMinutes; got != 1440 ||
		DefaultTunerPlanMaxAgeMinutes != 1440 {
		t.Fatalf("plan_max_age_minutes default = %d (const %d), want 1440", got,
			DefaultTunerPlanMaxAgeMinutes)
	}
}

func TestLoad_PlanMaxAgeMinutesFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "mode: standalone\npostgres:\n  host: localhost\n" +
		"tuner:\n  plan_max_age_minutes: 90\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Tuner.PlanMaxAgeMinutes != 90 {
		t.Fatalf("plan_max_age_minutes = %d, want 90", cfg.Tuner.PlanMaxAgeMinutes)
	}
	keyless := []byte("tuner:\n  enabled: true\n")
	if err := os.WriteFile(path, keyless, 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load([]string{"--config", path}); err != nil ||
		cfg.Tuner.PlanMaxAgeMinutes != 1440 {
		t.Fatalf("a file without the key keeps 1440: %v %v", cfg, err)
	}
}
