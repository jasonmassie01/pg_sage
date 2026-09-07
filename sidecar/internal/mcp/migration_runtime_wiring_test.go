package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestApplyMigrationUsesConfiguredRehearsalRuntime(t *testing.T) {
	store := &recordingIntentStore{}
	runtime := &recordingSafeMigrationRuntime{result: migrationruntime.Result{
		Verdict:    migrationruntime.VerdictExpanded,
		EvidenceID: "ev-expanded", ContractNotBeforeCycle: 8,
	}}
	executor := NewProductionIntentExecutor(store, plan.NewPlanner(),
		&recordingProductionGate{decision: policy.Decision{
			Verdict: policy.VerdictExecute,
		}}).
		WithMigrationRuntime(runtime)
	request := policy.ActionRequest{
		DatabaseID: int64ProductionPointer(42),
		Contract:   &policy.ActionContract{ActionType: "apply_migration"},
		Arguments: json.RawMessage(`{
			"table":"public.users","cycle":7,
			"sql":"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"
		}`),
	}

	result, err := executor.ExecuteConcrete(context.Background(), request)
	require.NoError(t, err)
	outcome := result.(MigrationOutcome)
	require.Equal(t, "expanded", outcome.Verdict)
	require.Equal(t, 8, outcome.ContractNotBeforeCycle)
	require.Equal(t, "ev-expanded", outcome.EvidenceID)
	require.True(t, outcome.Plan.Rewritten)
	require.Equal(t, 1, runtime.calls)
	require.Equal(t, 7, runtime.request.Cycle)
	require.Equal(t, int64(42), *runtime.request.DatabaseID)
	require.Equal(t, "users", runtime.request.Table.Name)
	require.Zero(t, store.ddlExecutions)
}

type recordingSafeMigrationRuntime struct {
	result  migrationruntime.Result
	err     error
	calls   int
	request migrationruntime.Request
}

func (runtime *recordingSafeMigrationRuntime) Apply(
	_ context.Context, request migrationruntime.Request,
) (migrationruntime.Result, error) {
	runtime.calls++
	runtime.request = request
	return runtime.result, runtime.err
}
