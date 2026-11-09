package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrent access tests: every consumer checked here is a pure
// function of the provider name.

// providerListExceptions are known providers a consumer leaves out on
// purpose. Every other known provider must be handled by every consumer.
var providerListExceptions = map[string]map[string]string{
	executor.ActionTypeGuardRoleEnsure: {"lakebase": "agent roles are not verified on Lakebase"},
	"cancel_backend": {"lakebase": "only owned-table contracts are verified on Lakebase " +
		"(lakebaseOwnedActionProviders)"},
}

// TestKnownProvidersCoverDetection pins knownProviders to what detection
// can return, so a new probe or hosted suffix fails until it is listed.
func TestKnownProvidersCoverDetection(t *testing.T) {
	known := map[string]bool{}
	for _, name := range knownProviders {
		if known[name] {
			t.Errorf("knownProviders lists %q twice", name)
		}
		known[name] = true
	}
	detected := []string{providerUnknown, providerSelfManaged}
	for _, probe := range providerProbes {
		detected = append(detected, probe.provider)
	}
	for _, host := range []string{
		"ep-x.database.us-east-1.cloud.databricks.com", "ep-x.us-east-2.aws.neon.tech",
		"db.p.supabase.co", "aws-0-us-east-1.pooler.supabase.com",
		"s.postgres.database.azure.com", "c-s.x.postgres.cosmos.azure.com",
	} {
		name := hostedProviderFromHost(host)
		if name == "" {
			t.Fatalf("sample host %q is not recognised", host)
		}
		detected = append(detected, name)
	}
	for _, name := range detected {
		if !known[name] {
			t.Errorf("detection can return %q but knownProviders omits it", name)
		}
	}
	if len(known) != len(dedupeProviders(detected)) {
		t.Errorf("knownProviders %v lists a name detection never returns (detected %v)",
			knownProviders, dedupeProviders(detected))
	}
}

// TestProviderConsumersAgree runs every detectable provider through each
// hand-maintained consumer and asserts they agree on managed versus
// self-managed versus unknown.
func TestProviderConsumersAgree(t *testing.T) {
	for _, name := range knownProviders {
		t.Run(name, func(t *testing.T) { checkProviderConsumers(t, name) })
	}
}

func checkProviderConsumers(t *testing.T, name string) {
	t.Helper()
	managed := advisor.IsManagedService(name)
	adapter := fleet.AdapterForProvider(name)
	canonical := adapter.Provider
	alterSystem := contractSupports(t, "alter_system_guc", canonical)
	switch name {
	case providerUnknown:
		if managed || canonical != "unknown" || len(adapter.SupportedActions) != 0 {
			t.Fatalf("unknown: managed %v adapter %q actions %d", managed, canonical,
				len(adapter.SupportedActions))
		}
		for action := range providerListExceptions {
			if contractSupports(t, action, canonical) {
				t.Errorf("unknown is supported by %s", action)
			}
		}
		return
	case providerSelfManaged:
		if managed || canonical != "postgres" || !alterSystem {
			t.Fatalf("self-managed: managed %v adapter %q alter_system %v",
				managed, canonical, alterSystem)
		}
	default:
		if !managed || alterSystem || canonical == "unknown" || canonical == "postgres" {
			t.Fatalf("%s: managed %v alter_system %v adapter %q", name, managed,
				alterSystem, canonical)
		}
		if adapter.LogAccess == "local" || adapter.LogAccess == "unknown" {
			t.Errorf("%s: adapter log access %q", name, adapter.LogAccess)
		}
	}
	for action, exceptions := range providerListExceptions {
		_, excepted := exceptions[canonical]
		if got := contractSupports(t, action, canonical); got == excepted {
			t.Errorf("%s: %s support %v, exception listed %v", name, action, got, excepted)
		}
	}
}

func contractSupports(t *testing.T, actionType, provider string) bool {
	t.Helper()
	contract, ok := executor.ContractForActionType(actionType)
	if !ok {
		t.Fatalf("no contract for %s", actionType)
	}
	for _, p := range contract.ProviderSupport {
		if strings.EqualFold(p, provider) {
			return true
		}
	}
	return false
}

func dedupeProviders(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
