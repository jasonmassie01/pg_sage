package rollout

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestRuntimeDueTriggerWaitsForVerifiedSuccessThreshold(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	evidence := &fakeRuntimeEvidenceSource{items: []PriorEvidence{
		priorEvidence("first", "success", now.Add(-time.Hour)),
	}}
	runs := &fakeRolloutRunStore{}
	runner := &recordingRuntimeEngine{result: Result{Instances: map[string]InstanceResult{}}}
	runtime := newTestRuntimeWithOptions(
		runner, evidence, runtimeInstances("db-a"), fixedRuntimePolicy(1, 1, 10),
		runs, now, RuntimeOptions{MinVerifiedSuccesses: 2, TriggerInterval: time.Hour},
	)

	triggered, err := runtime.RunDue(context.Background())

	require.NoError(t, err)
	require.False(t, triggered)
	require.Empty(t, runner.requests)

	evidence.items = append(evidence.items,
		priorEvidence("second", "success", now.Add(-30*time.Minute)))
	triggered, err = runtime.RunDue(context.Background())

	require.NoError(t, err)
	require.True(t, triggered)
	require.Len(t, runner.requests, 1)
	require.Equal(t, "second", runner.requests[0].Prior.EvidenceID)
	require.Equal(t, []string{"canary", "complete"}, runs.states())
}

func TestRuntimeDueTriggerHonorsPeriodicInterval(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	evidence := &fakeRuntimeEvidenceSource{items: []PriorEvidence{
		priorEvidence("new-prior", "success", now.Add(-time.Minute)),
	}}
	runs := &fakeRolloutRunStore{records: []RunRecord{{
		EvidenceID: "previous-run", State: "complete", UpdatedAt: now.Add(-30 * time.Minute),
	}}}
	runner := &recordingRuntimeEngine{result: Result{Instances: map[string]InstanceResult{}}}
	runtime := newTestRuntimeWithOptions(
		runner, evidence, runtimeInstances("db-a"), fixedRuntimePolicy(1, 1, 10),
		runs, now, RuntimeOptions{MinVerifiedSuccesses: 1, TriggerInterval: time.Hour},
	)

	triggered, err := runtime.RunDue(context.Background())

	require.NoError(t, err)
	require.False(t, triggered)
	require.Empty(t, runner.requests)
}

func TestRuntimeCancellationStopsPeriodicTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &recordingRuntimeEngine{}
	runtime := newTestRuntime(
		runner,
		&fakeRuntimeEvidenceSource{items: []PriorEvidence{
			priorEvidence("prior", "success", time.Now()),
		}},
		runtimeInstances("db-a"), fixedRuntimePolicy(1, 1, 10), time.Now(),
	)

	triggered, err := runtime.RunDue(ctx)

	require.ErrorIs(t, err, context.Canceled)
	require.False(t, triggered)
	require.Empty(t, runner.requests)
}
