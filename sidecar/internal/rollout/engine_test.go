package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestRolloutUsesKInstanceCanaryBeforeFleetExpansion(t *testing.T) {
	backend := newRecordingRolloutBackend()
	engine := NewEngine(backend, backend)
	request := healthyRolloutRequest("a", "b", "c", "d", "e")
	request.Policy.CanaryInstances = 2

	result, err := engine.Rollout(context.Background(), request)

	require.NoError(t, err)
	require.False(t, result.Halted)
	require.Equal(t, 5, result.AppliedInstances)
	require.Equal(t, []string{"a", "b"}, result.CanaryInstanceIDs)
	require.Equal(t, []string{
		"reverify:a", "apply:a", "measure:a",
		"reverify:b", "apply:b", "measure:b",
		"reverify:c", "apply:c", "measure:c",
		"reverify:d", "apply:d", "measure:d",
		"reverify:e", "apply:e", "measure:e",
	}, backend.events)
	for _, instanceID := range []string{"a", "b", "c", "d", "e"} {
		require.Equal(t, 1, backend.reverifyCalls[instanceID])
		require.Equal(t, 1, backend.applyCalls[instanceID])
		require.Equal(t, 1, backend.measureCalls[instanceID])
	}
}

func TestRolloutHaltsAfterCanaryAggregateRegression(t *testing.T) {
	backend := newRecordingRolloutBackend()
	backend.outcomes["a"] = Outcome{RegressionPct: 10, EvidenceID: "verify-a"}
	backend.outcomes["b"] = Outcome{RegressionPct: 30, EvidenceID: "verify-b"}
	engine := NewEngine(backend, backend)
	request := healthyRolloutRequest("a", "b", "c", "d")
	request.Policy.CanaryInstances = 2
	request.Policy.AggregateRegressionLimitPct = 15

	result, err := engine.Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "aggregate_regression", result.HaltReason)
	require.Equal(t, float64(20), result.AggregateRegressionPct)
	require.Equal(t, 2, result.AppliedInstances)
	require.Zero(t, backend.reverifyCalls["c"])
	require.Zero(t, backend.applyCalls["c"])
	require.Zero(t, backend.reverifyCalls["d"])
	require.Zero(t, backend.applyCalls["d"])
}

func TestRolloutContainsFleetBlastRadius(t *testing.T) {
	backend := newRecordingRolloutBackend()
	engine := NewEngine(backend, backend)
	request := healthyRolloutRequest("a", "b", "c", "d", "e", "f")
	request.Policy.CanaryInstances = 2
	request.Policy.MaxAffectedInstances = 3

	result, err := engine.Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "blast_radius_limit", result.HaltReason)
	require.Equal(t, 3, result.AppliedInstances)
	require.Equal(t, 3, backend.totalApplyCalls())
	require.Zero(t, backend.applyCalls["d"])
	require.Zero(t, backend.applyCalls["e"])
	require.Zero(t, backend.applyCalls["f"])
}

func TestPriorIsReverifiedPerInstanceAndNeverBlindApplied(t *testing.T) {
	backend := newRecordingRolloutBackend()
	backend.reverification["b"] = Reverification{
		Eligible: false, Reason: "different_plan_shape",
	}
	backend.reverification["c"] = Reverification{
		Eligible: true, LocalEvidenceID: "local-c",
	}
	engine := NewEngine(backend, backend)
	request := healthyRolloutRequest("b", "c")
	request.Prior = Prior{
		EvidenceID:          "prior-a",
		ValidatedInstanceID: "a",
		Intent: json.RawMessage(
			`{"kind":"optimize_query","query_id":991}`,
		),
	}
	request.Policy.CanaryInstances = 1

	result, err := engine.Rollout(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, 1, backend.reverifyCalls["b"])
	require.Zero(t, backend.applyCalls["b"])
	require.Equal(t, 1, backend.reverifyCalls["c"])
	require.Equal(t, 1, backend.applyCalls["c"])
	require.Equal(t, "prior-a", backend.receivedPriors["b"].EvidenceID)
	require.Equal(t, "prior-a", backend.receivedPriors["c"].EvidenceID)
	require.Equal(t, "local-c", backend.appliedCandidates["c"].LocalEvidenceID)
	require.Equal(t, 1, result.AppliedInstances)
	require.Equal(t, "not_locally_verified", result.Instances["b"].Status)
}

func TestRolloutRejectsMissingValidatedPrior(t *testing.T) {
	backend := newRecordingRolloutBackend()
	request := healthyRolloutRequest("a", "b")
	request.Prior = Prior{}

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.ErrorIs(t, err, ErrPriorRequired)
	require.Zero(t, result.AppliedInstances)
	require.Zero(t, backend.totalCalls())
}

func TestRolloutStopsOnCancellationBeforeFurtherInstances(t *testing.T) {
	backend := newRecordingRolloutBackend()
	ctx, cancel := context.WithCancel(context.Background())
	backend.measureFunc = func(
		_ context.Context, instance Instance, _ AppliedChange,
	) (Outcome, error) {
		if instance.ID == "a" {
			cancel()
		}
		return Outcome{EvidenceID: "verify-" + instance.ID}, nil
	}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(ctx, request)

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, result.AppliedInstances)
	require.Equal(t, 1, backend.applyCalls["a"])
	require.Zero(t, backend.reverifyCalls["b"])
	require.Zero(t, backend.applyCalls["b"])
	require.Zero(t, backend.reverifyCalls["c"])
	require.Zero(t, backend.applyCalls["c"])
}

func TestRolloutFailsClosedOnLocalVerificationError(t *testing.T) {
	backend := newRecordingRolloutBackend()
	backend.reverifyErrors["a"] = errors.New("local verifier unavailable")
	request := healthyRolloutRequest("a", "b")

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.ErrorContains(t, err, "local verifier")
	require.Zero(t, result.AppliedInstances)
	require.Zero(t, backend.totalApplyCalls())
}

func healthyRolloutRequest(instanceIDs ...string) Request {
	instances := make([]Instance, 0, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		instances = append(instances, Instance{ID: instanceID, Class: "standard"})
	}
	return Request{
		Prior: Prior{
			EvidenceID:          "prior-source",
			ValidatedInstanceID: "source-instance",
			Intent: json.RawMessage(
				`{"kind":"optimize_query","query_id":991}`,
			),
		},
		Instances: instances,
		Policy: Policy{
			CanaryInstances:             2,
			MaxAffectedInstances:        len(instances),
			AggregateRegressionLimitPct: 15,
			RequireLocalReverification:  true,
		},
	}
}

type recordingRolloutBackend struct {
	reverification    map[string]Reverification
	reverifyErrors    map[string]error
	outcomes          map[string]Outcome
	receivedPriors    map[string]Prior
	appliedCandidates map[string]Reverification
	reverifyCalls     map[string]int
	applyCalls        map[string]int
	measureCalls      map[string]int
	events            []string
	measureFunc       func(context.Context, Instance, AppliedChange) (Outcome, error)
}

func newRecordingRolloutBackend() *recordingRolloutBackend {
	return &recordingRolloutBackend{
		reverification:    make(map[string]Reverification),
		reverifyErrors:    make(map[string]error),
		outcomes:          make(map[string]Outcome),
		receivedPriors:    make(map[string]Prior),
		appliedCandidates: make(map[string]Reverification),
		reverifyCalls:     make(map[string]int),
		applyCalls:        make(map[string]int),
		measureCalls:      make(map[string]int),
	}
}

func (backend *recordingRolloutBackend) Reverify(
	_ context.Context, instance Instance, prior Prior,
) (Reverification, error) {
	backend.events = append(backend.events, "reverify:"+instance.ID)
	backend.reverifyCalls[instance.ID]++
	backend.receivedPriors[instance.ID] = prior
	if err := backend.reverifyErrors[instance.ID]; err != nil {
		return Reverification{}, err
	}
	if result, ok := backend.reverification[instance.ID]; ok {
		return result, nil
	}
	return Reverification{
		Eligible: true, LocalEvidenceID: "local-" + instance.ID,
	}, nil
}

func (backend *recordingRolloutBackend) Apply(
	_ context.Context, instance Instance, candidate Reverification,
) (AppliedChange, error) {
	backend.events = append(backend.events, "apply:"+instance.ID)
	backend.applyCalls[instance.ID]++
	backend.appliedCandidates[instance.ID] = candidate
	return AppliedChange{Handle: "change-" + instance.ID}, nil
}

func (backend *recordingRolloutBackend) Measure(
	ctx context.Context, instance Instance, change AppliedChange,
) (Outcome, error) {
	backend.events = append(backend.events, "measure:"+instance.ID)
	backend.measureCalls[instance.ID]++
	if backend.measureFunc != nil {
		return backend.measureFunc(ctx, instance, change)
	}
	if result, ok := backend.outcomes[instance.ID]; ok {
		return result, nil
	}
	return Outcome{EvidenceID: "verify-" + instance.ID}, nil
}

func (backend *recordingRolloutBackend) totalApplyCalls() int {
	total := 0
	for _, count := range backend.applyCalls {
		total += count
	}
	return total
}

func (backend *recordingRolloutBackend) totalCalls() int {
	total := 0
	for _, calls := range []map[string]int{
		backend.reverifyCalls, backend.applyCalls, backend.measureCalls,
	} {
		for _, count := range calls {
			total += count
		}
	}
	return total
}
