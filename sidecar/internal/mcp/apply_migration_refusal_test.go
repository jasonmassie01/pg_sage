package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func executingGate() *recordingProductionGate {
	return &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictExecute, EvidenceID: "ev-refusal"}}
}

// gatedMigrationServer serves apply_migration through the production
// backend with a gate that would let the migration run.
func gatedMigrationServer(t *testing.T, store *recordingIntentStore) *Server {
	t.Helper()
	return productionServer(t, func(d *ProductionDependencies) {
		d.Planner = DeterministicIntentPlanner{}
		d.Executor = NewProductionIntentExecutor(store, plan.NewPlanner(), executingGate())
	})
}

// migration-02b: SQL the online-migration planner refuses is a request
// the caller must change (-32602) carrying the planner's reason, never an
// opaque internal error, and nothing is recorded or run.
func TestApplyMigrationPlannerRefusalIsInvalidArguments(t *testing.T) {
	store := &recordingIntentStore{}
	response := invoke(t, gatedMigrationServer(t, store), adminContext(),
		toolCall("apply_migration", `{"table":"public.users","cycle":1,"sql":`+
			`"ALTER TABLE public.users ALTER COLUMN a SET NOT NULL, ALTER COLUMN b SET NOT NULL"}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, response.Error.Message)
	require.Contains(t, response.Error.Message, "single ALTER TABLE clause")
	require.Contains(t, response.Error.Message, "build safe migration plan")
	require.Equal(t, MigrationRecord{}, store.migration)
	require.Zero(t, store.ddlExecutions)
}

// A cancelled request stays a cancellation, not a refusal to fix.
func TestApplyMigrationCancelledPlanIsNotInvalid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner(),
		executingGate())
	_, err := executor.ExecuteConcrete(ctx, migrationRequest())
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, ErrIntentInvalid), "cancellation reported as invalid: %v",
		err)
}
