package fleet

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

// No concurrent access tests: adapters are built fresh per call and share
// no references.

func TestAdapterForProviderAzureCosmos(t *testing.T) {
	for _, name := range []string{"azure-cosmos", " AZURE-COSMOS "} {
		a := AdapterForProvider(name)
		if a.Provider != "azure-cosmos" {
			t.Fatalf("%q: provider = %q, want azure-cosmos", name, a.Provider)
		}
		if a.LogAccess != "azure_monitor" {
			t.Errorf("%q: log access = %q, want azure_monitor", name, a.LogAccess)
		}
		assertLimitation(t, a.Limitations, "guidance-only (Citus coordinator)")
		assertLimitation(t, a.Limitations, "azure.extensions")
		for _, l := range a.Limitations {
			if strings.Contains(l, "change through Azure") {
				t.Errorf("cosmos lists the flexible-server limitation %q", l)
			}
		}
		for action, want := range map[string]bool{
			"alter_database_guc": false, "alter_system_guc": false,
			"analyze_table": true, "create_index_concurrently": true,
		} {
			if got := a.SupportsAction(action); got != want {
				t.Errorf("%q: SupportsAction(%s) = %v, want %v", name, action, got, want)
			}
		}
	}
}

func TestAdapterForProviderAzureFlexibleUnchanged(t *testing.T) {
	for _, name := range []string{"azure", "azure-flexible", "azure-single"} {
		a := AdapterForProvider(name)
		if a.Provider != "azure" || !a.SupportsAction("alter_database_guc") {
			t.Fatalf("%q: provider %q, alter_database_guc %v", name, a.Provider,
				a.SupportsAction("alter_database_guc"))
		}
		assertLimitation(t, a.Limitations, "change through Azure")
	}
}

// State: readiness for a Cosmos database carries the Cosmos limitations and
// blocks ALTER DATABASE through the adapter, not through the gate.
func TestBuildProviderCapabilitiesAzureCosmos(t *testing.T) {
	caps := BuildProviderCapabilities("azure-cosmos", false, nil)
	if caps.Provider != "azure-cosmos" || caps.LogAccess != "azure_monitor" {
		t.Fatalf("provider %q log %q", caps.Provider, caps.LogAccess)
	}
	assertLimitation(t, caps.Limitations, "guidance-only (Citus coordinator)")
	for _, l := range caps.Limitations {
		if strings.Contains(l, "server parameters change through Azure") {
			t.Fatalf("cosmos readiness shows the flexible-server limitation %q", l)
		}
	}
	family := findFamily(t, caps.ActionFamilies, "alter_database_guc")
	if family.Supported || family.Decision != executor.PolicyDecisionBlocked ||
		family.BlockedReason != "provider adapter does not support action" {
		t.Fatalf("alter_database_guc readiness = %+v", family)
	}
}

func TestNormalizeProviderNameKeepsCosmosDistinct(t *testing.T) {
	cases := map[string]string{
		"azure-cosmos": "azure-cosmos", "AZURE-COSMOS ": "azure-cosmos",
		"azure-flexible": "azure", "azure-single": "azure", "azure": "azure",
		"": "unknown", "unknown": "unknown", "self-managed": "postgres",
	}
	for in, want := range cases {
		if got := normalizeProviderName(in); got != want {
			t.Errorf("normalizeProviderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func assertLimitation(t *testing.T, limitations []string, want string) {
	t.Helper()
	for _, l := range limitations {
		if strings.Contains(l, want) {
			return
		}
	}
	t.Errorf("limitations %q lack %q", limitations, want)
}

func findFamily(
	t *testing.T, families []ActionFamilyReadiness, action string,
) ActionFamilyReadiness {
	t.Helper()
	for _, f := range families {
		if f.ActionType == action {
			return f
		}
	}
	t.Fatalf("no readiness family %s", action)
	return ActionFamilyReadiness{}
}

// Aliases resolve through the provider registry; an unrecognised name is
// kept for display but gets the fail-closed unknown adapter.
func TestAdapterForProviderRegistryAliases(t *testing.T) {
	for in, want := range map[string]string{"aws": "rds", "GCP": "cloud-sql",
		"aurora-postgresql": "aurora", "postgresql": "postgres", "self-managed": "postgres"} {
		if got := AdapterForProvider(in); got.Provider != want || got.LogAccess == "unknown" {
			t.Errorf("AdapterForProvider(%q) = %q (log %q), want %q", in, got.Provider,
				got.LogAccess, want)
		}
	}
	for in, want := range map[string]string{"bogus": "bogus", " Bogus ": "bogus", "": "unknown",
		"unknown": "unknown"} {
		a := AdapterForProvider(in)
		if a.Provider != want || a.LogAccess != "unknown" || len(a.SupportedActions) != 0 {
			t.Errorf("AdapterForProvider(%q) = %q log %q actions %d", in, a.Provider,
				a.LogAccess, len(a.SupportedActions))
		}
	}
}
