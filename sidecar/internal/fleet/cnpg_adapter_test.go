package fleet

import (
	"strings"
	"testing"
)

// market-12: a CloudNativePG Cluster runs plain PostgreSQL, so the SQL
// action families stay, but its operator owns postgresql.conf: readiness
// says so instead of showing an adapter with no limitations.

// No concurrent access tests: AdapterForProvider reads static tables only.

func TestCNPGProviderAdapter(t *testing.T) {
	for _, name := range []string{"cnpg", "CNPG", "cloudnative-pg", "cloudnativepg"} {
		adapter := AdapterForProvider(name)
		if adapter.Provider != "cnpg" || adapter.LogAccess != "kubernetes" {
			t.Fatalf("AdapterForProvider(%q) = %q, log access %q", name, adapter.Provider,
				adapter.LogAccess)
		}
		for _, action := range []string{"analyze_table", "alter_database_guc", "cancel_backend"} {
			if !adapter.SupportsAction(action) {
				t.Errorf("%s: cnpg adapter does not support %s", name, action)
			}
		}
		if adapter.SupportsAction("alter_system_guc") {
			t.Fatalf("%s: the operator disables ALTER SYSTEM, but the adapter allows it", name)
		}
		all := strings.Join(adapter.Limitations, " | ")
		for _, want := range []string{"spec.postgresql.parameters", "ALTER SYSTEM",
			"shared_preload_libraries", "rolling restart"} {
			if !strings.Contains(all, want) {
				t.Errorf("%s: limitations %q lack %q", name, adapter.Limitations, want)
			}
		}
		for _, ext := range []string{"pg_hint_plan", "auto_explain"} {
			if adapter.Extensions[ext] != "cluster_spec_required" {
				t.Errorf("%s: %s readiness = %q, want cluster_spec_required", name, ext,
					adapter.Extensions[ext])
			}
		}
	}
}
