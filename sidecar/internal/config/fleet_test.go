package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: Load builds a private Config per call.

const fleetIdentityYAML = `mode: fleet
databases:
  - name: orders
    host: 10.0.0.5
    port: 5432
    user: sage
    database: orders
    provider: rds
    cloud_resource:
      aws:
        db_instance_identifier: orders-prod
        region: us-east-1
  - name: billing
    host: 127.0.0.1
    port: 5433
    user: sage
    database: billing
    provider: cloud-sql
    cloud_resource:
      gcp:
        project: proj-1
        instance: billing-main
  - name: plain
    host: db.internal
    port: 5432
    user: sage
    database: plain
`

// Each fleet database carries its own override and resource.
func TestFleetDatabasesCarryTheirOwnIdentity(t *testing.T) {
	cfg, err := loadYAMLResult(t, fleetIdentityYAML)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byName := map[string]DatabaseConfig{}
	for _, db := range cfg.Databases {
		byName[db.Name] = db
	}
	orders := byName["orders"]
	if orders.Provider != "rds" || orders.CloudResource.AWS.DBInstanceIdentifier !=
		"orders-prod" || orders.CloudResource.AWS.Region != "us-east-1" ||
		orders.CloudResource.Cloud() != "aws" {
		t.Fatalf("orders identity = %q %+v", orders.Provider, orders.CloudResource)
	}
	billing := byName["billing"]
	if billing.Provider != "cloud-sql" || billing.CloudResource.GCP.Instance != "billing-main" ||
		billing.CloudResource.AWS != (CloudResourceAWS{}) {
		t.Fatalf("billing identity = %q %+v", billing.Provider, billing.CloudResource)
	}
	plain := byName["plain"]
	if plain.Provider != "" || plain.CloudResource != (CloudResourceConfig{}) {
		t.Fatalf("plain identity leaked: %q %+v", plain.Provider, plain.CloudResource)
	}
}

func TestFleetDatabaseInvalidIdentityNamesTheDatabase(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"unknown provider", "    provider: openstack\n",
			`databases["a"].provider "openstack" is not a provider`},
		{"resource of the wrong cloud", "    provider: rds\n    cloud_resource:\n" +
			"      gcp:\n        instance: main\n", "does not describe provider \"rds\""},
		{"aws without a name", "    cloud_resource:\n      aws:\n" +
			"        region: us-east-1\n", "names no resource"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAMLResult(t, "mode: fleet\ndatabases:\n  - name: a\n"+
				"    host: h\n    port: 5432\n    user: u\n    database: d\n"+tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The older cloud_telemetry names stay standalone-only; a fleet database
// names its resource through databases[].cloud_resource instead.
func TestFleetCloudTelemetryNamesStayStandaloneOnly(t *testing.T) {
	_, err := loadYAMLResult(t, fleetIdentityYAML+"cloud_telemetry:\n  aws:\n"+
		"    db_instance_identifier: orders\n")
	if err == nil || !strings.Contains(err.Error(), "standalone only") {
		t.Fatalf("Load error = %v, want the standalone-only refusal", err)
	}
}
