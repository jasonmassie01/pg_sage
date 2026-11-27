package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// market-12: CloudNativePG is managed for configuration. ALTER SYSTEM is
// refused with the Cluster-spec guidance, and pg_sage never patches the
// Cluster (no adapter is ever used).

// No concurrent access tests: these are pure functions of the provider.

func TestCNPGIsManagedForConfig(t *testing.T) {
	for _, name := range []string{"cnpg", "CNPG", "cloudnative-pg"} {
		if !isManagedProvider(name) {
			t.Errorf("isManagedProvider(%q) = false", name)
		}
	}
	if got := managedConfigMechanism("cnpg"); got != ManagedProviderConfig {
		t.Fatalf("mechanism = %q, want provider_config", got)
	}
	g := managedConfigGuidance("cnpg", "shared_buffers")
	for _, want := range []string{"CloudNativePG", "spec.postgresql.parameters",
		"kubectl patch cluster", "shared_buffers", "ALTER SYSTEM is disabled"} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance %q lacks %q", g, want)
		}
	}
}

// The WAL-slot custodian never reaches an adapter on CNPG, even one that is
// wired: pg_sage does not write the Cluster.
func TestCNPGCustodianConfigIsUnsupported(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = "cnpg"
	e := New(nil, cfg, zeroTime(), nopLog)
	adapter := &countingAdapter{}
	e.managedConfig = adapter
	_, handled, err := e.applyManagedCustodianConfig(context.Background(),
		CustodianProposal{SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '10GB'"})
	if !handled || !errors.Is(err, ErrManagedConfigUnsupported) || adapter.calls != 0 {
		t.Fatalf("handled %v err %v adapter calls %d", handled, err, adapter.calls)
	}
}

func TestCNPGContractsKeepSQLActions(t *testing.T) {
	for _, action := range []string{"create_index_concurrently", "drop_unused_index",
		"vacuum_table", "analyze_table", "alter_database_guc"} {
		contract, ok := ContractForActionType(action)
		if !ok {
			t.Fatalf("no %s contract", action)
		}
		if !strings.Contains(strings.Join(contract.ProviderSupport, ","), "cnpg") {
			t.Errorf("%s: CNPG runs plain SQL actions like self-managed; providers %v",
				action, contract.ProviderSupport)
		}
	}
	alter, ok := ContractForActionType("alter_system_guc")
	if !ok {
		t.Fatal("no alter_system_guc contract")
	}
	for _, p := range alter.ProviderSupport {
		if p == "cnpg" {
			t.Fatal("alter_system_guc must not support CNPG")
		}
	}
}

type countingAdapter struct{ calls int }

func (c *countingAdapter) ApplyParameter(context.Context,
	ManagedConfigChange) (ManagedConfigResult, error) {
	c.calls++
	return ManagedConfigResult{InEffect: true}, nil
}
