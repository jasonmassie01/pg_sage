package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestUnavailableCloneDowngradesToRecommendationWithoutDDL(t *testing.T) {
	wrapper := recommendOnlyOnCloneFailure{delegate: failingCommandRehearser{}}
	result, err := wrapper.Rehearse(context.Background(), plan.Plan{
		ExpandSteps: []plan.Step{{SQL: "CREATE INDEX CONCURRENTLY idx ON t(id)"}},
	})
	require.NoError(t, err)
	require.Equal(t, rehearsal.VerdictRecommendOnly, result.Verdict)
	require.Equal(t, rehearsal.Reason("clone_unavailable"), result.Reason)
}

func TestConfiguredMCPCloneProviderNoneDisablesDDLRuntime(t *testing.T) {
	provider, err := configuredMCPCloneProvider(config.CloneProviderConfig{
		Provider: "none", MaxCloneAgeMinutes: 60,
	})
	require.NoError(t, err)
	require.Nil(t, provider)
}

func TestConfiguredMCPCloneProviderDLEAndSnapshotSelection(t *testing.T) {
	dle, err := configuredMCPCloneProvider(config.CloneProviderConfig{
		Provider: "dle", DLEEndpoint: "https://database-lab.example.test",
		DLEToken: "test-token", MaxCloneAgeMinutes: 60,
	})
	require.NoError(t, err)
	require.IsType(t, &clone.DLEProvider{}, dle)

	original := mcpSnapshotProviderFactory
	t.Cleanup(func() { mcpSnapshotProviderFactory = original })
	expected := &commandCloneProvider{}
	mcpSnapshotProviderFactory = func(config.CloneProviderConfig) (clone.Provider, error) {
		return expected, nil
	}
	snapshot, err := configuredMCPCloneProvider(config.CloneProviderConfig{
		Provider: "snapshot", MaxCloneAgeMinutes: 60,
	})
	require.NoError(t, err)
	require.Same(t, expected, snapshot)
}

func TestFleetMCPApplyMigrationInjectsPerDatabaseConfiguredRuntime(t *testing.T) {
	runtime := &commandMigrationRuntime{result: migrationruntime.Result{
		Verdict: migrationruntime.VerdictRecommendOnly,
		Reason:  "stale_clone", ContractNotBeforeCycle: 5,
	}}
	factoryCalls := 0
	executor := &fleetIntentExecutor{
		access: &fleetMCPAccess{fallback: &pgxpool.Pool{}},
		gate: &mcpGateRecorder{decision: policy.Decision{
			Verdict: policy.VerdictExecute, EvidenceID: "ev-command-gate",
		}},
		cloneConfig: config.CloneProviderConfig{Provider: "dle"},
		migrationFactory: func(
			*pgxpool.Pool, policy.Gate, config.CloneProviderConfig,
		) (mcp.MigrationRuntime, error) {
			factoryCalls++
			return runtime, nil
		},
	}

	result, err := executor.ExecuteConcrete(context.Background(), policy.ActionRequest{
		Contract: &policy.ActionContract{ActionType: "apply_migration"},
		Arguments: json.RawMessage(`{
			"table":"public.users","cycle":4,
			"sql":"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"
		}`),
	})
	require.NoError(t, err)
	outcome := result.(mcp.MigrationOutcome)
	require.Equal(t, "recommend_only", outcome.Verdict)
	require.Equal(t, 1, factoryCalls)
	require.Equal(t, 1, runtime.calls)
}

func TestPerDatabaseMigrationGatePreservesFleetDatabaseIdentity(t *testing.T) {
	databaseID := int64(42)
	capture := &commandGateCapture{}
	decision := (databaseBoundMCPGate{
		delegate: capture, databaseID: &databaseID,
	}).Authorize(context.Background(), policy.ActionRequest{
		SQL: "CREATE INDEX CONCURRENTLY idx ON public.orders(id)",
	})
	require.Equal(t, policy.VerdictExecute, decision.Verdict)
	require.NotNil(t, capture.request.DatabaseID)
	require.Equal(t, databaseID, *capture.request.DatabaseID)
}

type commandMigrationRuntime struct {
	result migrationruntime.Result
	calls  int
}

type commandGateCapture struct {
	request policy.ActionRequest
}

func (capture *commandGateCapture) Authorize(
	_ context.Context, request policy.ActionRequest,
) policy.Decision {
	capture.request = request
	return policy.Decision{Verdict: policy.VerdictExecute}
}

func (runtime *commandMigrationRuntime) Apply(
	context.Context, migrationruntime.Request,
) (migrationruntime.Result, error) {
	runtime.calls++
	return runtime.result, nil
}

type commandCloneProvider struct{}

type failingCommandRehearser struct{}

func (failingCommandRehearser) Rehearse(
	context.Context, plan.Plan,
) (rehearsal.Result, error) {
	return rehearsal.Result{}, errors.New("clone control plane unavailable")
}

func (*commandCloneProvider) Create(
	context.Context, clone.CloneSpec,
) (clone.Clone, error) {
	return clone.Clone{}, nil
}
func (*commandCloneProvider) Destroy(context.Context, clone.Clone) error { return nil }
func (*commandCloneProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}
