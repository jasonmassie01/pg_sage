package rollout

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestRuntimeSelectsNewestValidatedPriorAndInheritedCohortPolicy(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	evidence := &fakeRuntimeEvidenceSource{items: []PriorEvidence{
		priorEvidence("older-success", "success", now.Add(-2*time.Hour)),
		priorEvidence("newest-reverted", "revert", now.Add(-time.Hour)),
		priorEvidence("newer-success", "success", now.Add(-90*time.Minute)),
	}}
	cohort := []Instance{
		{ID: "db-a", Class: "production", Tags: []string{"critical"}},
		{ID: "db-b", Class: "production", Tags: []string{"critical"}},
	}
	runner := &recordingRuntimeEngine{result: Result{Instances: map[string]InstanceResult{}}}
	runtime := newTestRuntime(runner, evidence, cohort, inheritedPolicySet(), now)

	_, err := runtime.RunOnce(context.Background())

	require.NoError(t, err)
	require.Len(t, runner.requests, 1)
	request := runner.requests[0]
	require.Equal(t, "newer-success", request.Prior.EvidenceID)
	require.Equal(t, cohort, request.Instances)
	require.Equal(t, 3, request.Policy.CanaryInstances)
	require.Equal(t, 7, request.Policy.MaxAffectedInstances)
	require.Equal(t, float64(9), request.Policy.AggregateRegressionLimitPct)
	require.Equal(t, []string{"fleet", "class:production", "tag:critical"},
		request.Policy.Sources)
}

func TestRuntimePersistsCanaryRegressionHaltFromRealEngine(t *testing.T) {
	backend := newRecordingRolloutBackend()
	backend.outcomes["db-a"] = Outcome{RegressionPct: 10, EvidenceID: "local-a"}
	backend.outcomes["db-b"] = Outcome{RegressionPct: 30, EvidenceID: "local-b"}
	engine := NewEngine(backend, backend)
	cohort := runtimeInstances("db-a", "db-b", "db-c", "db-d")
	policies := fixedRuntimePolicy(2, 4, 15)
	runs := &fakeRolloutRunStore{}
	runtime := runtimeWithStore(engine, cohort, policies, runs)

	record, err := runtime.RunOnce(context.Background())

	require.NoError(t, err)
	require.Equal(t, "halted", record.State)
	require.Equal(t, "aggregate_regression", record.HaltReason)
	require.Equal(t, []string{"db-a", "db-b"}, record.CanaryInstanceIDs)
	require.Equal(t, 2, record.AppliedInstances)
	require.Equal(t, float64(20), record.AggregateRegressionPct)
	require.Zero(t, backend.reverifyCalls["db-c"])
	require.Equal(t, []string{"canary", "halted"}, runs.states())
}

func TestRuntimeEnforcesBlastRadiusAndReverifiesEveryAppliedInstance(t *testing.T) {
	backend := newRecordingRolloutBackend()
	engine := NewEngine(backend, backend)
	cohort := runtimeInstances("db-a", "db-b", "db-c", "db-d", "db-e")
	runs := &fakeRolloutRunStore{}
	runtime := runtimeWithStore(engine, cohort, fixedRuntimePolicy(2, 3, 50), runs)

	record, err := runtime.RunOnce(context.Background())

	require.NoError(t, err)
	require.Equal(t, "halted", record.State)
	require.Equal(t, "blast_radius_limit", record.HaltReason)
	require.Equal(t, 3, record.AppliedInstances)
	for _, id := range []string{"db-a", "db-b", "db-c"} {
		require.Equal(t, 1, backend.reverifyCalls[id])
		require.Equal(t, 1, backend.applyCalls[id])
	}
	require.Zero(t, backend.reverifyCalls["db-d"])
	require.Equal(t, []string{"canary", "halted"}, runs.states())
}

func TestRuntimePersistsFailureWhenRolloutIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &recordingRuntimeEngine{run: func(
		context.Context, Request,
	) (Result, error) {
		cancel()
		return Result{}, context.Canceled
	}}
	runs := &fakeRolloutRunStore{}
	runtime := runtimeWithStore(
		runner, runtimeInstances("db-a"), fixedRuntimePolicy(1, 1, 10), runs,
	)

	record, err := runtime.RunOnce(ctx)

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "failed", record.State)
	require.Equal(t, "context_canceled", record.HaltReason)
	require.Equal(t, []string{"canary", "failed"}, runs.states())
}

func TestRuntimeFailsClosedWhenNoValidatedPriorExists(t *testing.T) {
	evidence := &fakeRuntimeEvidenceSource{items: []PriorEvidence{
		priorEvidence("pending", "pending", time.Now()),
		priorEvidence("reverted", "revert", time.Now().Add(time.Minute)),
	}}
	runner := &recordingRuntimeEngine{}
	runtime := newTestRuntime(
		runner, evidence, runtimeInstances("db-a"),
		fixedRuntimePolicy(1, 1, 10), time.Now(),
	)

	_, err := runtime.RunOnce(context.Background())

	require.ErrorIs(t, err, ErrPriorRequired)
	require.Empty(t, runner.requests)
}

func priorEvidence(id, verdict string, completedAt time.Time) PriorEvidence {
	return PriorEvidence{
		Prior: Prior{
			EvidenceID: id, ValidatedInstanceID: "source-" + id,
			Intent: json.RawMessage(`{"kind":"optimize_query","query_id":991}`),
		},
		Verdict: verdict, CompletedAt: completedAt,
	}
}

func runtimeInstances(ids ...string) []Instance {
	instances := make([]Instance, 0, len(ids))
	for _, id := range ids {
		instances = append(instances, Instance{ID: id, Class: "production"})
	}
	return instances
}
