package managedparam

import "testing"

// NormalizeProvider reads the provider registry: every registry alias of a
// parameter-group, database-flag or Azure server-parameter provider
// resolves; guidance-only, tenant, retired (azure-single) and unknown
// providers do not. advisors-03 added AlloyDB and Azure flexible server.
func TestNormalizeProviderRegistryAliases(t *testing.T) {
	cases := map[string]string{
		"aws-rds": "rds", "aurora-postgresql": "aurora", "cloud sql": "cloud-sql",
		"gcp-cloud-sql": "cloud-sql", " GCP ": "cloud-sql",
		"azure-flexible": "azure", "azure-postgres": "azure", "alloydb": "alloydb",
		"azure-cosmos": "", "azure-single": "", "lakebase": "", "supabase": "",
		"self-managed": "", "postgres": "", "unknown": "", "rds;": "",
	}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}
