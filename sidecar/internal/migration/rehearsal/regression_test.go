package rehearsal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestOrchestratorEmptyWorkloadIsInconclusive is the G7-B22 / R08
// regression: with no affected-query evidence the regression gate was
// vacuously passed and the plan promoted.
func TestOrchestratorEmptyWorkloadIsInconclusive(t *testing.T) {
	provider := freshProvider()
	runner := &fakeRunner{measurement: Measurement{
		MaxLockDuration: 20 * time.Millisecond, DiskDeltaBytes: 4096,
	}}
	result, err := NewOrchestrator(provider, runner, testOptions()).Rehearse(
		context.Background(), rehearsalPlan())
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if result.Verdict == VerdictPromoteExpand {
		t.Fatalf("empty workload promoted: %#v", result)
	}
	if result.Verdict != VerdictRecommendOnly ||
		result.Reason != ReasonInconclusiveWorkload {
		t.Fatalf("Result = %#v, want recommend_only/%s", result,
			ReasonInconclusiveWorkload)
	}
	if result.Measurement.DiskDeltaBytes != 4096 {
		t.Fatalf("measurement evidence dropped: %#v", result.Measurement)
	}
}

// TestOrchestratorRecordsCloneCleanupFailure is the second half of
// R08: Destroy errors were discarded (leaked clone, apparent success).
func TestOrchestratorRecordsCloneCleanupFailure(t *testing.T) {
	provider := freshProvider()
	provider.destroyErr = errors.New("dle: clone busy")
	runner := &fakeRunner{measurement: passingMeasurement()}
	result, err := NewOrchestrator(provider, runner, testOptions()).Rehearse(
		context.Background(), rehearsalPlan())
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if !strings.Contains(result.CleanupError, "clone busy") ||
		!strings.Contains(result.CleanupError, "clone-1") {
		t.Fatalf("CleanupError = %q, want clone id and cause",
			result.CleanupError)
	}
}

// TestOrchestratorClassifiesErrors is the G7-B25 library half: clone
// provider failures and step failures must be distinguishable.
func TestOrchestratorClassifiesErrors(t *testing.T) {
	provider := freshProvider()
	provider.createErr = errors.New("no capacity")
	_, err := NewOrchestrator(provider, &fakeRunner{}, testOptions()).
		Rehearse(context.Background(), rehearsalPlan())
	if !errors.Is(err, ErrCloneUnavailable) || errors.Is(err, ErrStepFailed) {
		t.Fatalf("create error = %v, want ErrCloneUnavailable only", err)
	}
	stepErr := errors.New("duplicate key")
	_, err = NewOrchestrator(freshProvider(), &fakeRunner{err: stepErr},
		testOptions()).Rehearse(context.Background(), rehearsalPlan())
	if !errors.Is(err, ErrStepFailed) || !errors.Is(err, stepErr) ||
		errors.Is(err, ErrCloneUnavailable) {
		t.Fatalf("runner error = %v, want ErrStepFailed wrapping cause", err)
	}
}
