package fleet

import (
	"testing"
	"time"
)

// No shared state: each adapter owns its maps. Connection error propagation and
// table-level permissions are exercised by the provider integration/live suites.
func TestHostedProviderAdapterPortableActions(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		t.Run(provider, func(t *testing.T) {
			adapter := AdapterForProvider(provider)
			if adapter.Provider != provider || adapter.LogAccess != "provider_console" {
				t.Fatalf("adapter identity/log access = %q/%q", adapter.Provider, adapter.LogAccess)
			}
			for _, action := range []string{
				"analyze_table", "vacuum_table", "create_index_concurrently",
				"diagnose_wal_replication", "cancel_backend", "reindex_concurrently",
				"alter_database_guc",
			} {
				if !adapter.SupportsAction(action) {
					t.Errorf("portable action %q not supported", action)
				}
			}
			if adapter.SupportsAction("alter_system_guc") || adapter.SupportsAction("bogus") {
				t.Fatal("adapter advertised an unavailable action")
			}
			for _, ext := range []string{"pg_stat_statements", "hypopg", "pg_hint_plan", "vector"} {
				if adapter.Extensions[ext] != "unknown" {
					t.Errorf("%s must await live evidence, got %q", ext, adapter.Extensions[ext])
				}
			}
			if len(adapter.Limitations) == 0 {
				t.Fatal("managed provider limitations missing")
			}
			adapter.Extensions["vector"] = "changed"
			if AdapterForProvider(provider).Extensions["vector"] != "unknown" {
				t.Fatal("adapter maps share mutable state")
			}
		})
	}
}

func TestHostedReadinessPreservesSafetyGates(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		for _, gate := range []string{"replica", "stop", "observation"} {
			t.Run(provider+"/"+gate, func(t *testing.T) {
				cfg := readinessTestConfig("autonomous")
				if gate == "observation" {
					cfg.Trust.Level = "observation"
				}
				caps := BuildProviderCapabilities(
					cfg, provider, gate == "replica", "auto", gate == "stop", time.Now())
				got := actionReadiness(t, caps.ActionFamilies, "analyze_table")
				if got.Decision == "execute" || caps.ReadyForAutoSafe {
					t.Fatalf("gate %s allowed execution: %#v", gate, got)
				}
			})
		}
	}
}
