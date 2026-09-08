package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestTenantProvidersPortableContracts(t *testing.T) {
	actions := []string{"analyze_table", "vacuum_table", "alter_database_guc",
		"create_index_concurrently", "drop_unused_index", "reindex_concurrently",
		"set_table_autovacuum", "diagnose_lock_blockers", "diagnose_wal_replication",
		"cancel_backend", "terminate_backend", "ddl_preflight", "alter_table"}
	for _, provider := range []string{"neon", "supabase"} {
		for _, action := range actions {
			t.Run(provider+"/"+action, func(t *testing.T) {
				contract, ok := ContractForActionType(action)
				if !ok || !providerSupported(provider, contract.ProviderSupport) {
					t.Fatalf("portable action %s excludes %s", action, provider)
				}
				if err := contract.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
		contract, _ := ContractForActionType("alter_system_guc")
		if providerSupported(provider, contract.ProviderSupport) {
			t.Fatalf("%s must not execute ALTER SYSTEM", provider)
		}
	}
}

func TestTenantProvidersPreservePolicyGates(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{CloudEnvironment: provider,
				Trust: config.TrustConfig{Level: "autonomous", Tier3Safe: true}}
			ctx := ActionPolicyContext{Config: cfg, ExecutionMode: "auto",
				Now: time.Now(), RampStart: time.Now().Add(-10 * 24 * time.Hour)}
			got := EvaluateActionPolicy(AnalyzeTableContract(), ctx)
			if got.Decision != PolicyDecisionExecute || got.Provider != provider {
				t.Fatalf("portable analyze decision: %#v", got)
			}
			ctx.EmergencyStop = true
			got = EvaluateActionPolicy(AnalyzeTableContract(), ctx)
			if got.BlockedReason != "emergency stop is active" {
				t.Fatalf("emergency stop lost: %#v", got)
			}
			ctx.EmergencyStop, ctx.IsReplica = false, true
			got = EvaluateActionPolicy(AnalyzeTableContract(), ctx)
			if got.BlockedReason != "target database is a replica" {
				t.Fatalf("replica gate lost: %#v", got)
			}
		})
	}
}

func TestTenantManagedConfigControlPaths(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		t.Run(provider, func(t *testing.T) {
			if !isManagedProvider(" " + strings.ToUpper(provider) + " ") {
				t.Fatal("managed provider was not recognized")
			}
			exec := newManagedConfigTestExecutor(provider)
			result, handled, err := exec.applyManagedCustodianConfig(
				context.Background(), managedWALProposal("10GB"))
			if !handled || result.InEffect || err == nil {
				t.Fatalf("missing control path: handled=%v result=%+v err=%v", handled, result, err)
			}
			if strings.Contains(err.Error(), "parameter group") {
				t.Fatalf("invented parameter group: %v", err)
			}
			if provider == "supabase" && !errors.Is(err, ErrManagedConfigAdapterUnavailable) {
				t.Fatalf("missing Supabase adapter: %v", err)
			}
		})
	}
}

func TestNeonCannotDelegateInstanceGUCToAnArbitraryAdapter(t *testing.T) {
	exec := newManagedConfigTestExecutor("neon")
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
	exec.WithManagedConfigAdapter(adapter)
	_, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"))
	if !handled || err == nil || adapter.calls != 0 {
		t.Fatalf("Neon instance restriction: handled=%v err=%v calls=%d", handled, err, adapter.calls)
	}
}

func TestSupabaseManagedConfigUsesProviderConfig(t *testing.T) {
	exec := newManagedConfigTestExecutor("supabase")
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
	exec.WithManagedConfigAdapter(adapter)
	_, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"))
	if err != nil || !handled || adapter.calls != 1 {
		t.Fatalf("Supabase control path: handled=%v err=%v calls=%d", handled, err, adapter.calls)
	}
	if got := string(adapter.changes[0].Mechanism); got != "provider_config" {
		t.Fatalf("mechanism=%q, want provider_config", got)
	}
}

// Database effects are covered by the opt-in provider integration harness.
// No concurrent tests: these policy/parser functions have no mutable shared state.
