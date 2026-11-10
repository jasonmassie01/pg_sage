package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: validation is a pure function of the config.

const subscriptionID = "0b1f6471-1bf0-4dda-aec3-111122223333"

func TestCloudIdentityAcceptsEveryProviderOverride(t *testing.T) {
	for _, provider := range append([]string{""}, ProviderOverrides...) {
		if err := validateCloudIdentity("postgres", provider,
			CloudResourceConfig{}); err != nil {
			t.Errorf("provider %q rejected: %v", provider, err)
		}
	}
	if len(ProviderOverrides) != 10 {
		t.Fatalf("ProviderOverrides = %v, want the ten detectable names", ProviderOverrides)
	}
}

type invalidIdentityCase struct {
	name, provider string
	resource       CloudResourceConfig
	want           string
}

func invalidIdentityCases() []invalidIdentityCase {
	return []invalidIdentityCase{
		{"unknown provider string", "heroku", CloudResourceConfig{}, `"heroku" is not a provider`},
		{"unknown is not an override", "unknown", CloudResourceConfig{}, "rely on detection"},
		{"case matters", "RDS", CloudResourceConfig{}, `"RDS" is not a provider`},
		{"two clouds", "", CloudResourceConfig{
			AWS: CloudResourceAWS{DBInstanceIdentifier: "orders"},
			GCP: CloudResourceGCP{Instance: "main"}}, "aws and gcp"},
		{"aws block on cloud-sql", "cloud-sql", CloudResourceConfig{
			AWS: CloudResourceAWS{DBInstanceIdentifier: "orders"}},
			`does not describe provider "cloud-sql"`},
		{"gcp block on self-managed", "self-managed", CloudResourceConfig{
			GCP: CloudResourceGCP{Instance: "main"}}, `provider "self-managed"`},
		{"aws instance and cluster", "rds", CloudResourceConfig{AWS: CloudResourceAWS{
			DBInstanceIdentifier: "orders", ClusterIdentifier: "shop"}}, "not both"},
		{"aws region alone", "", CloudResourceConfig{
			AWS: CloudResourceAWS{Region: "us-east-1"}}, "names no resource"},
		{"aws bad identifier", "", CloudResourceConfig{
			AWS: CloudResourceAWS{DBInstanceIdentifier: "Orders_1"}},
			`db_instance_identifier "Orders_1"`},
		{"aws bad region", "", CloudResourceConfig{AWS: CloudResourceAWS{
			DBInstanceIdentifier: "orders", Region: "useast1"}}, `region "useast1"`},
		{"gcp bad project", "", CloudResourceConfig{
			GCP: CloudResourceGCP{Project: "p", Instance: "main"}}, `project "p"`},
		{"gcp injected instance", "", CloudResourceConfig{
			GCP: CloudResourceGCP{Instance: "main/../x"}}, `instance "main/../x"`},
		{"azure partial", "azure", CloudResourceConfig{
			Azure: CloudResourceAzure{Server: "flex-1"}}, "together"},
		{"azure bad subscription", "", CloudResourceConfig{Azure: CloudResourceAzure{
			Subscription: "not-a-uuid", ResourceGroup: "rg", Server: "flex-1"}},
			`subscription "not-a-uuid"`},
		{"azure resource group ends in a period", "", CloudResourceConfig{
			Azure: CloudResourceAzure{Subscription: subscriptionID,
				ResourceGroup: "rg.", Server: "flex-1"}}, `resource_group "rg."`},
		{"azure bad server", "", CloudResourceConfig{Azure: CloudResourceAzure{
			Subscription: subscriptionID, ResourceGroup: "rg", Server: "Flex_1"}},
			`server "Flex_1"`},
	}
}

func TestCloudIdentityRejectsInvalidInput(t *testing.T) {
	for _, tc := range invalidIdentityCases() {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCloudIdentity("databases[\"a\"]", tc.provider, tc.resource)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), `databases["a"]`) {
				t.Fatalf("error %q does not name the field", err)
			}
		})
	}
}

func TestCloudIdentityAcceptsValidResources(t *testing.T) {
	cases := []struct {
		provider string
		resource CloudResourceConfig
	}{
		{"rds", CloudResourceConfig{AWS: CloudResourceAWS{
			DBInstanceIdentifier: "orders", Region: "us-east-1"}}},
		{"aurora", CloudResourceConfig{AWS: CloudResourceAWS{ClusterIdentifier: "shop"}}},
		{"", CloudResourceConfig{GCP: CloudResourceGCP{Project: "proj-1", Instance: "main"}}},
		{"cloud-sql", CloudResourceConfig{GCP: CloudResourceGCP{Project: "proj-1"}}},
		{"azure", CloudResourceConfig{Azure: CloudResourceAzure{
			Subscription: strings.ToUpper(subscriptionID), ResourceGroup: "rg-(prod).1",
			Server: "flex-1"}}},
	}
	for _, tc := range cases {
		if err := validateCloudIdentity("postgres", tc.provider, tc.resource); err != nil {
			t.Errorf("%q %+v rejected: %v", tc.provider, tc.resource, err)
		}
	}
}

// Boundary: an RDS identifier is at most 63 characters.
func TestCloudIdentityIdentifierLengthBoundary(t *testing.T) {
	ok := "a" + strings.Repeat("b", 62)
	if err := validateAWSResource("x", CloudResourceAWS{DBInstanceIdentifier: ok}); err != nil {
		t.Fatalf("63 characters rejected: %v", err)
	}
	if err := validateAWSResource("x",
		CloudResourceAWS{DBInstanceIdentifier: ok + "c"}); err == nil {
		t.Fatal("64 characters accepted")
	}
}

func TestCloudResourceCloud(t *testing.T) {
	if got := (CloudResourceConfig{}).Cloud(); got != "" {
		t.Fatalf("empty Cloud() = %q", got)
	}
	r := CloudResourceConfig{GCP: CloudResourceGCP{Instance: "main"}}
	if got := r.Cloud(); got != "gcp" {
		t.Fatalf("Cloud() = %q, want gcp", got)
	}
	r.Azure.Server = "flex-1"
	if got := r.Cloud(); got != "" {
		t.Fatalf("two clouds Cloud() = %q, want empty", got)
	}
}

// Without the keys nothing is overridden and nothing is named.
func TestCloudIdentityDefaultsAreEmpty(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Postgres.Provider != "" || cfg.Postgres.CloudResource.Cloud() != "" {
		t.Fatalf("postgres identity = %q %+v, want empty", cfg.Postgres.Provider,
			cfg.Postgres.CloudResource)
	}
	for _, db := range cfg.Databases {
		if db.Provider != "" || db.CloudResource != (CloudResourceConfig{}) {
			t.Fatalf("database %q identity = %q %+v, want empty", db.Name, db.Provider,
				db.CloudResource)
		}
	}
}

// Standalone carries postgres: identity onto the synthesized database.
func TestStandalonePostgresIdentityReachesItsDatabase(t *testing.T) {
	cfg, err := loadYAMLResult(t, "mode: standalone\npostgres:\n  host: 127.0.0.1\n"+
		"  provider: cloud-sql\n  cloud_resource:\n    gcp:\n      project: proj-1\n"+
		"      instance: main\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Databases) != 1 {
		t.Fatalf("databases = %+v", cfg.Databases)
	}
	db := cfg.Databases[0]
	if db.Provider != "cloud-sql" || db.CloudResource.GCP.Project != "proj-1" ||
		db.CloudResource.GCP.Instance != "main" {
		t.Fatalf("synthesized database identity = %q %+v", db.Provider, db.CloudResource)
	}
}

func TestStandaloneInvalidProviderFailsLoad(t *testing.T) {
	_, err := loadYAMLResult(t, "mode: standalone\npostgres:\n  host: db\n"+
		"  provider: digitalocean\n")
	if err == nil || !strings.Contains(err.Error(), `postgres.provider "digitalocean"`) {
		t.Fatalf("Load error = %v, want the invalid provider named", err)
	}
}

func TestIdentityFieldsAreRestartAndRebuildBound(t *testing.T) {
	for _, path := range []string{"postgres.provider",
		"postgres.cloud_resource.gcp.instance"} {
		meta, ok := LookupFieldLifecycle(path)
		if !ok || meta.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (registered %v), want restart", path, meta, ok)
		}
	}
	for _, field := range []string{"provider", "cloud_resource.aws.db_instance_identifier"} {
		meta, ok := LookupDatabaseFieldLifecycle(field)
		if !ok || meta.Lifecycle != LifecycleReconfigure {
			t.Errorf("databases[].%s lifecycle = %+v (registered %v), want reconfigure",
				field, meta, ok)
		}
	}
	change := ClassifyDatabaseChange(DatabaseConfig{Name: "a"},
		DatabaseConfig{Name: "a", Provider: "rds"})
	if !change.Rebuild || len(change.Fields) != 1 || change.Fields[0] != "provider" {
		t.Fatalf("provider change = %+v, want a rebuild of provider", change)
	}
}
