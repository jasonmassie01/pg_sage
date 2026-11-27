package rollout

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Sage SRE M7 fleet canary (R06 finished): one database first, measure,
// then widen; any failed verification or a regression past the limit
// halts the rollout and rolls back every instance it changed, newest
// first, even when the caller's context ends.

type rollbackBackend struct {
	*recordingRolloutBackend
	rollbackErr map[string]error
	rolledBack  []string
}

func newRollbackBackend() *rollbackBackend {
	return &rollbackBackend{recordingRolloutBackend: newRecordingRolloutBackend(),
		rollbackErr: map[string]error{}}
}

func (b *rollbackBackend) Rollback(ctx context.Context, instance Instance,
	_ AppliedChange) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b.rolledBack = append(b.rolledBack, instance.ID)
	b.events = append(b.events, "rollback:"+instance.ID)
	return b.rollbackErr[instance.ID]
}

func TestAggregateRegressionRollsBackTheCanaries(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["a"] = Outcome{RegressionPct: 10, EvidenceID: "verify-a"}
	backend.outcomes["b"] = Outcome{RegressionPct: 30, EvidenceID: "verify-b"}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 2

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "aggregate_regression", result.HaltReason)
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Equal(t, 2, result.RolledBack)
	require.Equal(t, StatusRolledBack, result.Instances["a"].Status)
	require.Equal(t, StatusRolledBack, result.Instances["b"].Status)
	require.Zero(t, backend.applyCalls["c"])
}

func TestInstanceRegressionAfterTheCanaryHaltsAndRollsBack(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["b"] = Outcome{RegressionPct: 30, EvidenceID: "verify-b"}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "instance_regression", result.HaltReason)
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Zero(t, backend.applyCalls["c"])
	require.Equal(t, 30.0, result.Instances["b"].RegressionPct)
}

func TestFailedVerificationHaltsAtOnce(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["a"] = Outcome{Failed: true, Detail: "post-check failed",
		EvidenceID: "verify-a"}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 2

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "verification_failed", result.HaltReason)
	require.Equal(t, []string{"a"}, backend.rolledBack)
	require.Zero(t, backend.applyCalls["b"])
	require.Equal(t, "post-check failed", result.Instances["a"].Detail)
}

func TestRollbackFailureIsReportedAndTheRestStillRollBack(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["b"] = Outcome{RegressionPct: 50}
	backend.rollbackErr["b"] = errors.New("rollback SQL failed")
	request := healthyRolloutRequest("a", "b")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Equal(t, StatusRollbackFailed, result.Instances["b"].Status)
	require.Equal(t, StatusRolledBack, result.Instances["a"].Status)
	require.Equal(t, 1, result.RolledBack)
	require.Len(t, result.RollbackErrors, 1)
	require.ErrorContains(t, errors.New(result.RollbackErrors[0]), "rollback SQL failed")
}

func TestApplierWithoutRollbackReportsItUnavailable(t *testing.T) {
	backend := newRecordingRolloutBackend()
	backend.outcomes["a"] = Outcome{RegressionPct: 90}
	request := healthyRolloutRequest("a", "b")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, StatusRollbackUnavailable, result.Instances["a"].Status)
	require.Zero(t, result.RolledBack)
}

func TestBlastRadiusHaltDoesNotRollBack(t *testing.T) {
	backend := newRollbackBackend()
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1
	request.Policy.MaxAffectedInstances = 2

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, "blast_radius_limit", result.HaltReason)
	require.Empty(t, backend.rolledBack)
	require.Equal(t, StatusApplied, result.Instances["b"].Status)
}

// A halt found while the caller's context ends still rolls back.
func TestRollbacksSurviveCancellation(t *testing.T) {
	backend := newRollbackBackend()
	ctx, cancel := context.WithCancel(context.Background())
	backend.measureFunc = func(_ context.Context, instance Instance,
		_ AppliedChange) (Outcome, error) {
		if instance.ID == "b" {
			cancel()
			return Outcome{Failed: true, Detail: "verification failed"}, nil
		}
		return Outcome{}, nil
	}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(ctx, request)

	require.NoError(t, err)
	require.Equal(t, "verification_failed", result.HaltReason)
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
}

// executor-04: a step error (apply, measure, cancellation) rolls back
// every instance already changed; a rollback that fails then leaves the
// instance applied_unverified with the rollback error, never just applied.

type erroringBackend struct {
	*rollbackBackend
	applyErr map[string]error
}

func newErroringBackend() *erroringBackend {
	return &erroringBackend{rollbackBackend: newRollbackBackend(), applyErr: map[string]error{}}
}

func (b *erroringBackend) Apply(ctx context.Context, instance Instance,
	candidate Reverification) (AppliedChange, error) {
	if err := b.applyErr[instance.ID]; err != nil {
		b.events = append(b.events, "apply-failed:"+instance.ID)
		return AppliedChange{}, err
	}
	return b.rollbackBackend.Apply(ctx, instance, candidate)
}

func TestApplyErrorRollsBackTheChangedInstances(t *testing.T) {
	backend := newErroringBackend()
	backend.applyErr["b"] = errors.New("lock_timeout on the target")
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.ErrorContains(t, err, "apply to b")
	require.ErrorContains(t, err, "lock_timeout on the target")
	require.Equal(t, []string{"a"}, backend.rolledBack)
	require.Equal(t, 1, result.RolledBack)
	require.Equal(t, StatusRolledBack, result.Instances["a"].Status)
	require.Equal(t, StatusFailed, result.Instances["b"].Status)
	require.ErrorContains(t, errors.New(result.Instances["b"].Detail), "lock_timeout")
	require.Zero(t, backend.applyCalls["c"])
	require.Zero(t, backend.reverifyCalls["c"])
}

func TestMeasureErrorRollsBackTheMeasuredInstanceToo(t *testing.T) {
	backend := newErroringBackend()
	backend.measureFunc = func(_ context.Context, instance Instance,
		_ AppliedChange) (Outcome, error) {
		if instance.ID == "b" {
			return Outcome{}, errors.New("target unreachable")
		}
		return Outcome{EvidenceID: "verify-" + instance.ID}, nil
	}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.ErrorContains(t, err, "measure b")
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Equal(t, 2, result.RolledBack)
	require.Equal(t, StatusRolledBack, result.Instances["a"].Status)
	require.Equal(t, StatusRolledBack, result.Instances["b"].Status)
	require.Equal(t, 2, result.AppliedInstances)
}

func TestRollbackFailureAfterAnErrorRecordsAppliedUnverified(t *testing.T) {
	backend := newErroringBackend()
	backend.applyErr["c"] = errors.New("executor disabled")
	backend.rollbackErr["a"] = errors.New("DROP INDEX timed out")
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.ErrorContains(t, err, "executor disabled")
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Equal(t, StatusRolledBack, result.Instances["b"].Status)
	require.Equal(t, StatusAppliedUnverified, result.Instances["a"].Status)
	require.ErrorContains(t, errors.New(result.Instances["a"].Detail), "DROP INDEX timed out")
	require.Equal(t, 1, result.RolledBack)
	require.Len(t, result.RollbackErrors, 1)
	require.ErrorContains(t, errors.New(result.RollbackErrors[0]), "a: DROP INDEX timed out")
}

// Cancellation between instances is an engine error too: the changed
// instances are rolled back on a context that outlives the caller's.
func TestCancellationBetweenInstancesRollsBack(t *testing.T) {
	backend := newErroringBackend()
	ctx, cancel := context.WithCancel(context.Background())
	backend.measureFunc = func(_ context.Context, instance Instance,
		_ AppliedChange) (Outcome, error) {
		if instance.ID == "b" {
			cancel()
		}
		return Outcome{EvidenceID: "verify-" + instance.ID}, nil
	}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(ctx, request)

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"b", "a"}, backend.rolledBack)
	require.Equal(t, StatusRolledBack, result.Instances["a"].Status)
	require.Equal(t, StatusRolledBack, result.Instances["b"].Status)
	require.Zero(t, backend.applyCalls["c"])
}

// A verdict still pending at its deadline halts without judging and
// without a rollback: the target's own monitor still owns the change.
func TestPendingVerificationHaltsWithoutRollback(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["b"] = Outcome{Pending: true, EvidenceID: "verify-b",
		Detail: "still monitoring at the deadline"}
	request := healthyRolloutRequest("a", "b", "c")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.True(t, result.Halted)
	require.Equal(t, "verification_pending", result.HaltReason)
	require.Empty(t, backend.rolledBack)
	require.Zero(t, result.RolledBack)
	require.Equal(t, StatusApplied, result.Instances["a"].Status)
	require.Equal(t, StatusVerificationPending, result.Instances["b"].Status)
	require.Equal(t, "still monitoring at the deadline", result.Instances["b"].Detail)
	require.Zero(t, backend.applyCalls["c"])
}

// A pending canary is not counted as a canary pass: the aggregate is not
// computed from it, so the canary list stays empty.
func TestPendingCanaryIsNotCountedAsACanary(t *testing.T) {
	backend := newRollbackBackend()
	backend.outcomes["a"] = Outcome{Pending: true}
	request := healthyRolloutRequest("a", "b")
	request.Policy.CanaryInstances = 1

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, "verification_pending", result.HaltReason)
	require.Empty(t, result.CanaryInstanceIDs)
	require.Zero(t, backend.applyCalls["b"])
}
