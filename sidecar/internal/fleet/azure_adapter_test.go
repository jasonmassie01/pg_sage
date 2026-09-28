package fleet

import "testing"

func TestAzureProviderAdapter(t *testing.T) {
	for _, name := range []string{"azure", "Azure", "azure-flexible"} {
		adapter := AdapterForProvider(name)
		if adapter.Provider != "azure" {
			t.Fatalf("AdapterForProvider(%q).Provider = %q", name, adapter.Provider)
		}
		if adapter.LogAccess != "azure_monitor" {
			t.Fatalf("LogAccess = %q, want azure_monitor", adapter.LogAccess)
		}
		for _, ext := range []string{"hypopg", "pg_hint_plan", "auto_explain"} {
			if adapter.Extensions[ext] != "azure_extensions_allowlist_required" {
				t.Errorf("%s readiness = %q", ext, adapter.Extensions[ext])
			}
		}
		for _, action := range []string{"analyze_table", "alter_database_guc", "cancel_backend"} {
			if !adapter.SupportsAction(action) {
				t.Errorf("azure adapter does not support %s", action)
			}
		}
		if adapter.SupportsAction("alter_system_guc") || len(adapter.Limitations) == 0 {
			t.Fatalf("azure adapter = %#v, want no ALTER SYSTEM and limitations", adapter)
		}
	}
}

func TestAzureAnalyzeFamilyReadyThroughGate(t *testing.T) {
	exec := gateExecutor(t, "autonomous")
	got := buildActionFamilyReadiness(ProviderCapabilities{Provider: "azure"},
		ExecutorFamilyExplainer(exec))
	analyze := actionReadiness(t, got, "analyze_table")
	if !analyze.Supported {
		t.Fatalf("azure analyze readiness = %#v", analyze)
	}
}
