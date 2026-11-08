package main

import "testing"

// No concurrent state: hostname classification is pure and shares no references.
// Live connection/permission behavior is covered by the hosted provider live suite.
func TestHostedProviderFromHost(t *testing.T) {
	cases := []struct{ host, want string }{
		{"ep-test.us-east-2.aws.neon.tech", "neon"},
		{"ep-test-pooler.us-east-2.aws.neon.tech", "neon"},
		{" EP-TEST.US-EAST-2.AWS.NEON.TECH. ", "neon"},
		{"db.project.supabase.co", "supabase"},
		{"aws-0-us-east-1.pooler.supabase.com", "supabase"},
		{"", ""}, {"localhost", ""}, {"127.0.0.1", ""}, {"::1", ""},
		{"notneon.tech", ""}, {"neon.tech.attacker.example", ""},
		{"supabase.co.attacker.example", ""}, {"notsupabase.co", ""},
		{"project.rds.amazonaws.com", ""}, {"customer.example", ""},
		{"postgres://user:password@db.project.supabase.co", ""},
		{"db.project.supabase.co:5432", ""}, {"bad/name.neon.tech", ""},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := hostedProviderFromHost(tc.host); got != tc.want {
				t.Fatalf("provider = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostedProviderNilPoolRemainsUnknown(t *testing.T) {
	if got := detectCloudEnv(nil); got != "unknown" {
		t.Fatalf("provider = %q, want unknown", got)
	}
}

// Azure Database for PostgreSQL flexible servers are recognised from the
// host before any SQL probe; lookalike hosts are not.
func TestHostedProviderFromHostAzure(t *testing.T) {
	for host, want := range map[string]string{
		"sage-prod.postgres.database.azure.com":        "azure",
		"SAGE-PROD.postgres.database.azure.com.":       "azure",
		"postgres.database.azure.com.attacker.example": "",
		"sage-prod.postgres.database.azure.com:5432":   "",
		"sage.privatelink.postgres.database.azure.com": "azure",
	} {
		if got := hostedProviderFromHost(host); got != want {
			t.Errorf("hostedProviderFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestHostedProviderFromHostLakebase(t *testing.T) {
	for host, want := range map[string]string{
		"ep-example.database.us-west-2.cloud.databricks.com":                  "lakebase",
		"ep-example-pooler.database.ap-southeast-2.cloud.databricks.com":      "lakebase",
		"EP-EXAMPLE.DATABASE.EU-WEST-3.CLOUD.DATABRICKS.COM.":                 "lakebase",
		"ep-example.database.eastus2.azuredatabricks.net":                     "lakebase",
		"ep-example-pooler.database.westeurope.azuredatabricks.net":           "lakebase",
		"ep-example.database.us-west-2.cloud.databricks.com.attacker.example": "",
		"ep-example.database.eastus2.azuredatabricks.net.attacker.example":    "",
		"database.us-west-2.cloud.databricks.com":                             "",
		"nested.ep-example.database.us-west-2.cloud.databricks.com":           "",
		"ep-example.database.us-west-2.cloud.databricks.com:5432":             "",
		"https://ep-example.database.eastus2.azuredatabricks.net":             "",
	} {
		if got := hostedProviderFromHost(host); got != want {
			t.Errorf("hostedProviderFromHost(%q) = %q, want %q", host, got, want)
		}
	}
}
