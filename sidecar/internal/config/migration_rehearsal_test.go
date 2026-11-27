package config

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: these settings are plain values read once
// at load; Load builds a fresh Config per call.

const mig06Base = `
mode: standalone
postgres:
  host: localhost
  database: app
`

func loadMig06(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	t.Setenv("SAGE_DATABASE_URL", "")
	return Load([]string{"--config", writeYAMLFile(t, mig06Base+extra)})
}

func mig06Values(cfg *Config) (time.Duration, time.Duration, float64) {
	return cfg.Migration.ContractDelay(), cfg.Migration.MaxLock(), cfg.Rehearsal.PlanFlipPct
}

// migration-06: with no config the contract waits 1h, a step may hold
// ACCESS EXCLUSIVE for 2000 ms, and a plan flip parks above +5%.
func TestMigrationRehearsalDefaults(t *testing.T) {
	delay, lock, flip := mig06Values(DefaultConfig())
	if delay != time.Hour || lock != 2*time.Second || flip != 5 {
		t.Fatalf("DefaultConfig: delay=%s lock=%s flip=%v", delay, lock, flip)
	}
	cfg, err := loadMig06(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if delay, lock, flip := mig06Values(cfg); delay != time.Hour ||
		lock != 2*time.Second || flip != 5 {
		t.Fatalf("loaded defaults: delay=%s lock=%s flip=%v", delay, lock, flip)
	}
}

func TestMigrationRehearsalAcceptsValues(t *testing.T) {
	cfg, err := loadMig06(t, `
migration:
  contract_delay_minutes: 90
  max_lock_ms: 500
rehearsal:
  plan_flip_pct: 2.5
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if delay, lock, flip := mig06Values(cfg); delay != 90*time.Minute ||
		lock != 500*time.Millisecond || flip != 2.5 {
		t.Fatalf("loaded: delay=%s lock=%s flip=%v", delay, lock, flip)
	}
}

// Invalid input: a zero or negative delay, and out-of-range lock and
// flip budgets, are refused at load with the key named; zero is not
// "use the default" for these keys.
func TestMigrationRehearsalRefusesInvalidValues(t *testing.T) {
	tests := map[string]struct{ yaml, key string }{
		"zero delay": {"migration:\n  contract_delay_minutes: 0\n",
			"migration.contract_delay_minutes"},
		"negative delay": {"migration:\n  contract_delay_minutes: -60\n",
			"migration.contract_delay_minutes"},
		"word delay":    {"migration:\n  contract_delay_minutes: soon\n", "soon"},
		"zero lock":     {"migration:\n  max_lock_ms: 0\n", "migration.max_lock_ms"},
		"negative lock": {"migration:\n  max_lock_ms: -5\n", "migration.max_lock_ms"},
		"zero flip":     {"rehearsal:\n  plan_flip_pct: 0\n", "rehearsal.plan_flip_pct"},
		"negative flip": {"rehearsal:\n  plan_flip_pct: -1\n", "rehearsal.plan_flip_pct"},
		"flip over 100": {"rehearsal:\n  plan_flip_pct: 100.5\n", "rehearsal.plan_flip_pct"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadMig06(t, test.yaml)
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("Load error = %v, want one naming %s", err, test.key)
			}
		})
	}
}

// Boundary: the smallest and largest accepted values.
func TestMigrationRehearsalBoundaries(t *testing.T) {
	cfg, err := loadMig06(t, "migration:\n  contract_delay_minutes: 1\n  max_lock_ms: 1\n"+
		"rehearsal:\n  plan_flip_pct: 100\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if delay, lock, flip := mig06Values(cfg); delay != time.Minute ||
		lock != time.Millisecond || flip != 100 {
		t.Fatalf("loaded: delay=%s lock=%s flip=%v", delay, lock, flip)
	}
}
