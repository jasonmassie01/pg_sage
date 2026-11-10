package managedparam

import "testing"

// NormalizeProvider reads the provider registry: every registry alias of a
// parameter-group or Cloud SQL provider resolves; guidance-only, tenant and
// unknown providers do not.
func TestNormalizeProviderRegistryAliases(t *testing.T) {
	cases := map[string]string{
		"aws-rds": "rds", "aurora-postgresql": "aurora", "cloud sql": "cloud-sql",
		"gcp-cloud-sql": "cloud-sql", " GCP ": "cloud-sql",
		"azure-cosmos": "", "azure-flexible": "", "lakebase": "", "supabase": "",
		"self-managed": "", "postgres": "", "unknown": "", "rds;": "",
	}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}
