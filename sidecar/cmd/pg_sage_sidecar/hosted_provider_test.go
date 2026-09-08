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
