package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

type managedConfigAdapterStub struct {
	result  ManagedConfigResult
	err     error
	calls   int
	changes []ManagedConfigChange
}

func (s *managedConfigAdapterStub) ApplyParameter(
	_ context.Context, change ManagedConfigChange,
) (ManagedConfigResult, error) {
	s.calls++
	s.changes = append(s.changes, change)
	return s.result, s.err
}

func TestManagedWALConfigFailsClosedWithoutAdapter(t *testing.T) {
	exec := newManagedConfigTestExecutor("rds")
	result, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"),
	)
	if !handled {
		t.Fatal("managed ALTER SYSTEM was not handled by the provider path")
	}
	if !errors.Is(err, ErrManagedConfigAdapterUnavailable) {
		t.Fatalf("error = %v, want ErrManagedConfigAdapterUnavailable", err)
	}
	if result.InEffect {
		t.Fatal("missing provider adapter reported the parameter in effect")
	}
}

func TestManagedWALConfigRoutesExactParameterAndValue(t *testing.T) {
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{
		InEffect: true,
		Note:     "RDS parameter group applied and effective",
	}}
	exec := newManagedConfigTestExecutor("rds")
	exec.WithManagedConfigAdapter(adapter)

	result, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"),
	)
	if err != nil {
		t.Fatalf("apply managed config: %v", err)
	}
	if !handled || !result.InEffect {
		t.Fatalf("handled=%v in_effect=%v, want true/true", handled, result.InEffect)
	}
	if adapter.calls != 1 || len(adapter.changes) != 1 {
		t.Fatalf("adapter calls=%d changes=%d, want 1/1", adapter.calls, len(adapter.changes))
	}
	change := adapter.changes[0]
	if change.Provider != "rds" || change.Mechanism != ManagedParameterGroup {
		t.Fatalf("provider/mechanism = %q/%q", change.Provider, change.Mechanism)
	}
	if change.Parameter != "max_slot_wal_keep_size" || change.Value != "10GB" {
		t.Fatalf("parameter/value = %q/%q", change.Parameter, change.Value)
	}
	if change.Reset {
		t.Fatal("SET request was marked as RESET")
	}
}

func TestManagedWALConfigSelectsProviderMechanism(t *testing.T) {
	tests := []struct {
		provider  string
		mechanism ManagedConfigMechanism
	}{
		{provider: "aurora", mechanism: ManagedParameterGroup},
		{provider: "cloud-sql", mechanism: ManagedDatabaseFlag},
		{provider: "alloydb", mechanism: ManagedDatabaseFlag},
		{provider: "azure-flexible", mechanism: ManagedServerParameter},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
			exec := newManagedConfigTestExecutor(tc.provider)
			exec.WithManagedConfigAdapter(adapter)
			_, handled, err := exec.applyManagedCustodianConfig(
				context.Background(), managedWALProposal("1024MB"),
			)
			if err != nil || !handled {
				t.Fatalf("handled=%v error=%v", handled, err)
			}
			if got := adapter.changes[0].Mechanism; got != tc.mechanism {
				t.Fatalf("mechanism = %q, want %q", got, tc.mechanism)
			}
		})
	}
}

func TestManagedWALConfigFailsClosedWhenProviderCannotConfirmEffect(t *testing.T) {
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{
		Note: "parameter group update is pending",
	}}
	exec := newManagedConfigTestExecutor("rds")
	exec.WithManagedConfigAdapter(adapter)

	result, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"),
	)
	if !handled || result.InEffect {
		t.Fatalf("handled=%v in_effect=%v, want true/false", handled, result.InEffect)
	}
	if !errors.Is(err, ErrManagedConfigNotEffective) {
		t.Fatalf("error = %v, want ErrManagedConfigNotEffective", err)
	}
}

func TestManagedWALConfigRejectsMalformedChangeBeforeAdapter(t *testing.T) {
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
	exec := newManagedConfigTestExecutor("cloud-sql")
	exec.WithManagedConfigAdapter(adapter)
	proposal := CustodianProposal{
		Feature: "wal",
		SQL:     "ALTER SYSTEM SET max_slot_wal_keep_size = current_setting('work_mem')",
	}

	_, handled, err := exec.applyManagedCustodianConfig(context.Background(), proposal)
	if !handled || err == nil {
		t.Fatalf("handled=%v error=%v, want handled failure", handled, err)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
}

func TestParseManagedConfigChangeRejectsUnsupportedForms(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "executor validation", sql: "DROP TABLE public.orders"},
		{name: "reset", sql: "ALTER SYSTEM RESET max_slot_wal_keep_size"},
		{name: "missing value", sql: "ALTER SYSTEM SET max_slot_wal_keep_size"},
		{name: "unsupported parameter", sql: "ALTER SYSTEM SET work_mem = '1MB'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			change, err := parseManagedConfigChange("rds", tc.sql)
			if err == nil {
				t.Fatalf("change = %+v, want rejection", change)
			}
		})
	}
}

func TestManagedWALConfigRejectsNonEnforcingBoundaries(t *testing.T) {
	for _, value := range []string{"0", "0MB", "-1"} {
		t.Run(value, func(t *testing.T) {
			adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
			exec := newManagedConfigTestExecutor("rds")
			exec.WithManagedConfigAdapter(adapter)

			_, handled, err := exec.applyManagedCustodianConfig(
				context.Background(), managedWALProposal(value),
			)
			if !handled || err == nil {
				t.Fatalf("handled=%v error=%v, want handled failure", handled, err)
			}
			if adapter.calls != 0 {
				t.Fatalf("adapter calls = %d, want 0", adapter.calls)
			}
		})
	}
}

func TestManagedWALConfigPropagatesAdapterError(t *testing.T) {
	wantErr := errors.New("provider API unavailable")
	adapter := &managedConfigAdapterStub{err: wantErr}
	exec := newManagedConfigTestExecutor("alloydb")
	exec.WithManagedConfigAdapter(adapter)

	_, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("1MB"),
	)
	if !handled || !errors.Is(err, wantErr) {
		t.Fatalf("handled=%v error=%v, want wrapped provider error", handled, err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestSelfManagedWALConfigDoesNotUseManagedAdapter(t *testing.T) {
	adapter := &managedConfigAdapterStub{result: ManagedConfigResult{InEffect: true}}
	exec := newManagedConfigTestExecutor("self-managed")
	exec.WithManagedConfigAdapter(adapter)

	_, handled, err := exec.applyManagedCustodianConfig(
		context.Background(), managedWALProposal("10GB"),
	)
	if err != nil || handled {
		t.Fatalf("handled=%v error=%v, want false/nil", handled, err)
	}
	if adapter.calls != 0 {
		t.Fatalf("self-managed path called adapter %d times", adapter.calls)
	}
}

func newManagedConfigTestExecutor(provider string) *Executor {
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = provider
	return New(nil, cfg, nil, zeroTime(), nil)
}

func managedWALProposal(value string) CustodianProposal {
	return CustodianProposal{
		Feature: "wal",
		SQL:     "ALTER SYSTEM SET max_slot_wal_keep_size = '" + value + "'",
	}
}
