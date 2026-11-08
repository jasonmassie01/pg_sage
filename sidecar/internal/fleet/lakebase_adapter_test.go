package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
)

func TestLakebaseReadinessUsesValidatedFamilies(t *testing.T) {
	adapter := AdapterForProvider("Lakebase")
	if adapter.Provider != "lakebase" || adapter.LogAccess != "unavailable" {
		t.Fatalf("Lakebase adapter = %#v", adapter)
	}
	for _, action := range []string{
		"analyze_table", "vacuum_table", "create_index_concurrently",
		"revert_created_index", "reindex_concurrently", "create_statistics",
		"revert_created_statistics", "alter_database_guc",
	} {
		contract, ok := executor.ContractForActionType(action)
		if !ok || !adapter.SupportsAction(action) {
			t.Fatalf("validated family %s has no adapter/contract", action)
		}
		found := false
		for _, p := range contract.ProviderSupport {
			found = found || p == "lakebase"
		}
		if !found {
			t.Fatalf("validated family %s contract excludes Lakebase", action)
		}
	}
	exec := gateExecutor(t, "autonomous")
	caps := BuildProviderCapabilities("lakebase", false, ExecutorFamilyExplainer(exec))
	analyze := actionReadiness(t, caps.ActionFamilies, "analyze_table")
	if !analyze.Supported || analyze.Decision != executor.PolicyDecisionExecute {
		t.Errorf("analyze = %#v", analyze)
	}
	// The adapter admits the family, but an unearned tuning class still fails
	// the real standing gate. Provider support must not bypass that policy.
	stats := actionReadiness(t, caps.ActionFamilies, "create_statistics")
	if stats.Decision != executor.PolicyDecisionBlocked || stats.BlockedReason != "trust_ramp_not_satisfied" {
		t.Errorf("statistics standing gate = %#v", stats)
	}
	for _, action := range []string{"alter_system_guc", "drop_unused_index", "cancel_backend", "apply_query_hint"} {
		got := actionReadiness(t, caps.ActionFamilies, action)
		if got.Supported || got.Decision != executor.PolicyDecisionBlocked {
			t.Errorf("unvalidated %s = %#v", action, got)
		}
	}
	if strings.Contains(strings.Join(caps.Limitations, " "), "adapter unknown") {
		t.Fatal("known provider still reported as unknown")
	}
}

func TestLakebaseReadinessRetainsOperatorGates(t *testing.T) {
	for _, gate := range []string{"observation", "replica", "stop"} {
		t.Run(gate, func(t *testing.T) {
			exec := gateExecutor(t, "autonomous")
			if gate == "observation" {
				_ = exec.SetTrustLevel("observation")
			}
			if gate == "stop" {
				exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
			}
			caps := BuildProviderCapabilities("lakebase", gate == "replica", ExecutorFamilyExplainer(exec))
			got := actionReadiness(t, caps.ActionFamilies, "analyze_table")
			if got.Decision == executor.PolicyDecisionExecute || caps.ReadyForAutoSafe {
				t.Fatalf("%s bypassed: %#v", gate, got)
			}
		})
	}
}
