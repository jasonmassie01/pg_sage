package config

import (
	"strings"
	"testing"
)

// Managed-cloud telemetry is on by default (it only reads provider
// metrics and is unavailable without credentials); limits default when
// zero.
func TestCloudTelemetryDefaults(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ct := cfg.CloudTelemetry
	if !ct.IsEnabled() {
		t.Fatal("cloud telemetry must default to enabled")
	}
	if ct.PollInterval().Seconds() != 60 || ct.MaxReplicaLag() != 30 ||
		ct.MinFreeStorage() != 10 || ct.MinStorageRunway() != 24 ||
		ct.MinAvailableMemory() != 5 {
		t.Fatalf("defaults = %+v interval=%v lag=%v free=%v runway=%v mem=%v", ct,
			ct.PollInterval(), ct.MaxReplicaLag(), ct.MinFreeStorage(),
			ct.MinStorageRunway(), ct.MinAvailableMemory())
	}
	if lifecycle, ok := LookupFieldLifecycle("cloud_telemetry.enabled"); !ok ||
		lifecycle.Lifecycle != LifecycleRestart {
		t.Fatalf("cloud_telemetry.enabled lifecycle = %+v, want restart", lifecycle)
	}
}

func TestCloudTelemetryFromYAMLAndEnv(t *testing.T) {
	cfg, err := loadYAMLResult(t, "cloud_telemetry:\n  enabled: false\n"+
		"  poll_interval_seconds: 300\n  max_replica_lag_seconds: 12.5\n"+
		"  aws:\n    region: eu-west-1\n    db_instance_identifier: orders\n"+
		"  gcp:\n    project: proj-1\n    instance: main\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ct := cfg.CloudTelemetry
	if ct.IsEnabled() || ct.PollInterval().Seconds() != 300 || ct.MaxReplicaLag() != 12.5 ||
		ct.AWS.Region != "eu-west-1" || ct.AWS.DBInstanceIdentifier != "orders" ||
		ct.GCP.Project != "proj-1" || ct.GCP.Instance != "main" {
		t.Fatalf("from yaml = %+v", ct)
	}
	t.Setenv("SAGE_CLOUD_TELEMETRY_ENABLED", "true")
	t.Setenv("SAGE_AWS_DB_INSTANCE_IDENTIFIER", "orders-env")
	t.Setenv("SAGE_GCP_PROJECT", "proj-env")
	t.Setenv("SAGE_GCP_CLOUDSQL_INSTANCE", "main-env")
	cfg, err = loadYAMLResult(t, "cloud_telemetry:\n  enabled: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ct = cfg.CloudTelemetry
	if !ct.IsEnabled() || ct.AWS.DBInstanceIdentifier != "orders-env" ||
		ct.GCP.Project != "proj-env" || ct.GCP.Instance != "main-env" {
		t.Fatalf("env must override yaml: %+v", ct)
	}
}

func TestCloudTelemetryValidation(t *testing.T) {
	cases := map[string]string{
		"negative lag":     "cloud_telemetry:\n  max_replica_lag_seconds: -1\n",
		"free over 100":    "cloud_telemetry:\n  min_free_storage_pct: 101\n",
		"memory over 100":  "cloud_telemetry:\n  min_available_memory_pct: 150\n",
		"negative runway":  "cloud_telemetry:\n  min_storage_runway_hours: -2\n",
		"interval too low": "cloud_telemetry:\n  poll_interval_seconds: 5\n",
		"bad instance":     "cloud_telemetry:\n  aws:\n    db_instance_identifier: 'a;b'\n",
		"bad region":       "cloud_telemetry:\n  aws:\n    region: mars-1\n",
		"bad gcp project":  "cloud_telemetry:\n  gcp:\n    project: P_1\n",
		"unknown key":      "cloud_telemetry:\n  secret_key: x\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadYAMLResult(t, yaml); err == nil {
				t.Fatalf("accepted invalid config:\n%s", yaml)
			}
		})
	}
}

// Explicit instance names apply to one database; fleet databases are
// identified from their own hosts.
func TestCloudTelemetryInstanceNamesAreStandaloneOnly(t *testing.T) {
	_, err := loadYAMLResult(t, "mode: fleet\ndatabases:\n  - name: a\n    host: h\n"+
		"    database: a\n    user: u\n    password: p\n"+
		"cloud_telemetry:\n  aws:\n    db_instance_identifier: orders\n")
	if err == nil || !strings.Contains(err.Error(), "cloud_telemetry") {
		t.Fatalf("fleet mode with an instance name: err = %v", err)
	}
}
