package agentdb

import (
	"context"
	"testing"
)

func TestHostedProviderProfilesAndReadiness(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		if !validProvider(provider) {
			t.Fatalf("provider %s missing", provider)
		}
		modes := map[string]bool{}
		for _, profile := range defaultSizeProfiles() {
			if profile.Provider == provider {
				modes[stringParam(profile.ProviderParams, "mode")] = true
			}
		}
		if !modes["branch"] || !modes["project"] {
			t.Fatalf("profiles missing for %s", provider)
		}
		found := false
		for _, row := range ProviderReadinessList(context.Background()) {
			if row.Provider == provider {
				found = true
				if row.Found || len(row.DisabledReasons) == 0 {
					t.Fatal("missing runtime marked ready")
				}
			}
		}
		if !found {
			t.Fatalf("readiness missing for %s", provider)
		}
	}
}

func TestHostedRuntimeRegistrationRequiresExplicitEnable(t *testing.T) {
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "1")
	t.Setenv("PG_SAGE_NEON_API_KEY", "synthetic-token")
	t.Setenv("PG_SAGE_SUPABASE_ACCESS_TOKEN", "synthetic-token")
	for _, provider := range []string{"neon", "supabase"} {
		flag := "PG_SAGE_ENABLE_NEON_RUNNER"
		if provider == "supabase" {
			flag = "PG_SAGE_ENABLE_SUPABASE_RUNNER"
		}
		t.Setenv(flag, "0")
		before, err := RuntimeRunnerRegistryFromEnv(context.Background()).ForProvider(provider)
		if err != nil || before.Name() != "dry_run" {
			t.Fatal("runner enabled without flag")
		}
		t.Setenv(flag, "1")
		after, err := RuntimeRunnerRegistryFromEnv(context.Background()).ForProvider(provider)
		if err != nil || after.Name() == "dry_run" {
			t.Fatalf("configured runner missing: %v", err)
		}
	}
}

func TestHostedNormalizedPlanRequiresPublicEndpointAndScopePolicy(t *testing.T) {
	for _, provider := range []string{"neon", "supabase"} {
		plan, err := BuildNormalizedLivePlan(LivePlanInput{
			DeploymentID: "task", Provider: provider, Operation: ProvisionOpCreate,
			Region: "us-east-1", SizeProfileID: "hosted", TTLSeconds: 60,
			StorageGB: 1, BudgetUSD: 1,
			ProviderParams: map[string]any{"mode": "project", "organization": "owned-org"},
		})
		if err != nil || !plan.PublicIP || plan.Account != "owned-org" {
			t.Fatalf("provider scope/network omitted from signed plan: %+v %v", plan, err)
		}
	}
}
