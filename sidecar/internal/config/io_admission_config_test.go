package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadYAMLResult(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load([]string{"-config", path})
}

const standaloneIOYAML = `
mode: standalone
postgres:
  host: localhost
  port: 5432
  user: postgres
  database: app
`

func assertIOAdmissionDefaults(t *testing.T, cfg *Config) {
	t.Helper()
	if cfg.Verify.IOBaselineDays != 7 {
		t.Errorf("verify.io_baseline_days = %d, want 7", cfg.Verify.IOBaselineDays)
	}
	if cfg.Verify.IOSampleDays != 14 {
		t.Errorf("verify.io_sample_retention_days = %d, want 14",
			cfg.Verify.IOSampleDays)
	}
	if cfg.Safety.DataIOCeilingPct != 70 || cfg.Safety.WALIOCeilingPct != 70 {
		t.Errorf("IO ceilings = %d/%d, want 70/70",
			cfg.Safety.DataIOCeilingPct, cfg.Safety.WALIOCeilingPct)
	}
	if cfg.Verify.IOCapacity != nil {
		t.Errorf("io_capacity = %+v, want no attestation by default", cfg.Verify.IOCapacity)
	}
}

func TestDefaultConfigIOAdmissionDefaults(t *testing.T) {
	assertIOAdmissionDefaults(t, DefaultConfig())
}

func TestLoadWithoutConfigFileKeepsIOAdmissionDefaults(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load([]string{"--pg-url", "postgres://u:p@db.example:5432/app"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertIOAdmissionDefaults(t, cfg)
}

func TestLoadYAMLWithoutIOKeysKeepsDefaults(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+"verify:\n  min_samples: 5\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertIOAdmissionDefaults(t, cfg)
}

func TestExplicitZeroBaselineDaysDisablesLearning(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+"verify:\n  io_baseline_days: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Verify.IOBaselineDays != 0 {
		t.Fatalf("explicit 0 was masked by the default: %d", cfg.Verify.IOBaselineDays)
	}
}

func TestStandaloneDeclaredIOCapacityLoads(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML+`verify:
  io_capacity:
    read_write_mbps: 1000
    wal_mbps: 250
safety:
  data_io_ceiling_pct: 60
  wal_io_ceiling_pct: 50
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	capacity := cfg.Verify.IOCapacity
	if capacity == nil || capacity.ReadWriteMBps != 1000 || capacity.WALMBps != 250 {
		t.Fatalf("io_capacity = %+v", capacity)
	}
	if cfg.Safety.DataIOCeilingPct != 60 || cfg.Safety.WALIOCeilingPct != 50 ||
		cfg.Safety.CPUCeilingPct != DefaultCPUCeilingPct {
		t.Fatalf("ceilings = %+v", cfg.Safety)
	}
}

func TestIOAdmissionConfigValidation(t *testing.T) {
	cases := map[string]string{
		"zero read_write":  "verify:\n  io_capacity:\n    read_write_mbps: 0\n    wal_mbps: 10\n",
		"negative wal":     "verify:\n  io_capacity:\n    read_write_mbps: 10\n    wal_mbps: -1\n",
		"missing wal":      "verify:\n  io_capacity:\n    read_write_mbps: 10\n",
		"unknown key":      "verify:\n  io_capacity:\n    read_write_mbps: 10\n    wal_mbps: 10\n    iops: 3\n",
		"negative days":    "verify:\n  io_baseline_days: -1\n",
		"retention short":  "verify:\n  io_baseline_days: 7\n  io_sample_retention_days: 6\n",
		"retention zero":   "verify:\n  io_sample_retention_days: 0\n",
		"data ceiling 0":   "safety:\n  data_io_ceiling_pct: 0\n",
		"data ceiling 101": "safety:\n  data_io_ceiling_pct: 101\n",
		"wal ceiling 101":  "safety:\n  wal_io_ceiling_pct: 101\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAMLResult(t, standaloneIOYAML+body); err == nil {
				t.Fatalf("invalid IO admission config accepted:\n%s", body)
			}
		})
	}
}

func TestIOCeilingBoundariesAccepted(t *testing.T) {
	for _, pct := range []string{"1", "100"} {
		cfg, err := loadYAMLResult(t, standaloneIOYAML+
			"safety:\n  data_io_ceiling_pct: "+pct+"\n  wal_io_ceiling_pct: "+pct+"\n")
		if err != nil || cfg.Safety.DataIOCeilingPct == 0 {
			t.Fatalf("ceiling %s rejected: %v", pct, err)
		}
	}
}

const fleetIOYAML = `
mode: fleet
databases:
  - name: orders
    host: orders.internal
    port: 5432
    user: sage
    database: orders
    verify:
      io_capacity:
        read_write_mbps: 500
        wal_mbps: 125
  - name: billing
    host: billing.internal
    port: 5432
    user: sage
    database: billing
`

func TestFleetPerDatabaseIOCapacity(t *testing.T) {
	cfg, err := loadYAMLResult(t, fleetIOYAML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	orders, billing := cfg.Databases[0].Verify.IOCapacity, cfg.Databases[1].Verify.IOCapacity
	if orders == nil || orders.ReadWriteMBps != 500 || orders.WALMBps != 125 {
		t.Fatalf("orders capacity = %+v", orders)
	}
	if billing != nil {
		t.Fatalf("billing inherited an attestation it never made: %+v", billing)
	}
	clone := Clone(cfg)
	clone.Databases[0].Verify.IOCapacity.ReadWriteMBps = 1
	if cfg.Databases[0].Verify.IOCapacity.ReadWriteMBps != 500 {
		t.Fatal("Clone aliases the per-database IO attestation")
	}
}

func TestFleetRejectsFleetWideIOCapacity(t *testing.T) {
	_, err := loadYAMLResult(t, fleetIOYAML+
		"verify:\n  io_capacity:\n    read_write_mbps: 10\n    wal_mbps: 10\n")
	if err == nil || !strings.Contains(err.Error(), "databases[].verify.io_capacity") {
		t.Fatalf("fleet-wide attestation accepted or unclear: %v", err)
	}
}

func TestFleetRejectsInvalidPerDatabaseIOCapacity(t *testing.T) {
	body := strings.Replace(fleetIOYAML, "read_write_mbps: 500", "read_write_mbps: 0", 1)
	_, err := loadYAMLResult(t, body)
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("invalid per-database attestation accepted or unnamed: %v", err)
	}
}
