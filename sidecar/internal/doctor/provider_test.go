package doctor

import "testing"

// No concurrent access tests: DetectProvider is a pure function.

// providerCase is one recorded host name and role list.
type providerCase struct {
	name  string
	facts ProviderFacts
	code  string
	prov  string
}

// providerCases are the hosts and roles for every platform.
var providerCases = []providerCase{
	{"rds by host", ProviderFacts{Host: "orders.c9akciq32.us-east-1.rds.amazonaws.com"},
		"RDS", "rds"},
	{"rds by role", ProviderFacts{Host: "10.0.0.5", Roles: []string{"rds_superuser"}},
		"RDS", "rds"},
	{"aurora by function", ProviderFacts{Host: "10.0.0.5",
		Roles: []string{"rds_superuser"}, Aurora: true}, "AUR", "aurora"},
	{"aurora cluster endpoint", ProviderFacts{
		Host: "orders.cluster-c9akciq32.us-east-1.rds.amazonaws.com"}, "AUR", "aurora"},
	{"cloud sql by role", ProviderFacts{Host: "34.70.1.2",
		Roles: []string{"cloudsqlsuperuser"}}, "GCP", "cloud-sql"},
	{"alloydb by role", ProviderFacts{Host: "10.1.2.3",
		Roles: []string{"alloydbsuperuser"}}, "GCP", "alloydb"},
	{"azure by host", ProviderFacts{Host: "orders.postgres.database.azure.com"},
		"AZ", "azure"},
	{"azure by role", ProviderFacts{Host: "10.2.0.4", Roles: []string{"azure_pg_admin"}},
		"AZ", "azure"},
	{"neon by host", ProviderFacts{Host: "ep-cool-dawn-123456.us-east-2.aws.neon.tech"},
		"NEON", "neon"},
	{"neon by role", ProviderFacts{Host: "db.example.com",
		Roles: []string{"neon_superuser"}}, "NEON", "neon"},
	{"supabase by host", ProviderFacts{Host: "db.abcdefghijkl.supabase.co"},
		"SUPA", "supabase"},
	{"supabase pooler", ProviderFacts{Host: "aws-0-us-east-1.pooler.supabase.com"},
		"SUPA", "supabase"},
	{"supabase by role", ProviderFacts{Host: "db.example.com",
		Roles: []string{"supabase_admin"}}, "SUPA", "supabase"},
	{"lakebase by host", ProviderFacts{
		Host: "instance-1a2b.database.us-west-2.cloud.databricks.com"}, "LB", "lakebase"},
	{"lakebase on azure", ProviderFacts{
		Host: "instance-1a2b.database.eastus.azuredatabricks.net"}, "LB", "lakebase"},
	{"lakebase by role", ProviderFacts{Host: "10.0.0.9",
		Roles: []string{"databricks_superuser"}}, "LB", "lakebase"},
	{"self-managed superuser", ProviderFacts{Host: "127.0.0.1", Superuser: true},
		"SM", "self-managed"},
	{"upper-case host with dot", ProviderFacts{Host: "ORDERS.POSTGRES.DATABASE.AZURE.COM."},
		"AZ", "azure"},
	{"unknown non-superuser", ProviderFacts{Host: "db.internal"}, "unknown", "unknown"},
	{"empty facts", ProviderFacts{}, "unknown", "unknown"},
	{"injected host is not trusted", ProviderFacts{Host: "x.neon.tech/../evil"},
		"unknown", "unknown"},
}

// Provider detection over recorded host names and role lists for every
// platform. Hosts are the providers' documented endpoint shapes; roles are
// the admin roles each creates.
func TestDetectProviderEveryPlatform(t *testing.T) {
	for _, tc := range providerCases {
		got := DetectProvider(tc.facts)
		if got.Code != tc.code || got.Provider != tc.prov || got.Name == "" {
			t.Errorf("%s: DetectProvider(%+v) = %+v, want %s/%s with a name",
				tc.name, tc.facts, got, tc.code, tc.prov)
		}
	}
}

// A managed role wins over superuser: a managed service never grants a
// real superuser, so superuser alone is the self-managed signal.
func TestDetectProviderManagedRoleBeatsSuperuser(t *testing.T) {
	got := DetectProvider(ProviderFacts{Host: "127.0.0.1", Superuser: true,
		Roles: []string{"azure_pg_admin"}})
	if got.Code != "AZ" {
		t.Fatalf("got %+v, want AZ", got)
	}
}

// The provider check passes for a detected platform and is unknown,
// with what was looked for, otherwise.
func TestProviderCheckStatus(t *testing.T) {
	if c := providerCheck(DetectProvider(ProviderFacts{Host: "a.neon.tech"})); c.Status !=
		StatusPass || c.Detail == "" {
		t.Fatalf("neon check = %+v, want pass", c)
	}
	c := providerCheck(DetectProvider(ProviderFacts{Host: "db.internal"}))
	if c.Status != StatusUnknown || c.Detail == "" {
		t.Fatalf("unknown check = %+v, want unknown with a detail", c)
	}
}

// ManagedRoles lists every role DetectProvider recognises.
func TestManagedRolesCoverDetection(t *testing.T) {
	for _, role := range ManagedRoles() {
		if DetectProvider(ProviderFacts{Roles: []string{role}}).Code == "unknown" {
			t.Errorf("role %s is probed but not recognised", role)
		}
	}
	if len(ManagedRoles()) < 7 {
		t.Fatalf("managed roles = %v, want one per managed platform", ManagedRoles())
	}
}
