package rehearsal

import (
	"context"
	"errors"
	"testing"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
)

// migration-07: a rehearsal is bounded (clone creation included), so the
// clone's receipt, which expires after the bound, is never reaped in use.

func TestRehearsalDefaultBoundIsOneHour(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	runner := &fakeRunner{measurement: passingMeasurement(),
		run: func(ctx context.Context, _ planpkg.Plan) error {
			deadline, hasDeadline = ctx.Deadline()
			return nil
		}}
	started := time.Now()
	_, err := NewOrchestrator(freshProvider(), runner, testOptions()).
		Rehearse(context.Background(), rehearsalPlan())
	if err != nil {
		t.Fatal(err)
	}
	if DefaultMaxDuration != time.Hour {
		t.Fatalf("DefaultMaxDuration = %s, want 1h", DefaultMaxDuration)
	}
	bound := deadline.Sub(started)
	if !hasDeadline || bound < 59*time.Minute || bound > 61*time.Minute {
		t.Fatalf("runner deadline %v (set %v), want about 1h", bound, hasDeadline)
	}
}

func TestRehearsalBoundStopsARunawayRunAndDestroysTheClone(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, _ planpkg.Plan) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	provider := freshProvider()
	options := testOptions()
	options.MaxDuration = 50 * time.Millisecond
	started := time.Now()
	result, err := NewOrchestrator(provider, runner, options).
		Rehearse(context.Background(), rehearsalPlan())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrStepFailed) {
		t.Fatalf("runaway rehearsal: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the bound did not stop the run: %s", elapsed)
	}
	if result.Verdict != VerdictRecommendOnly || provider.destroyCalls != 1 ||
		provider.destroyContextErr != nil {
		t.Fatalf("result %+v destroy calls %d ctx %v", result, provider.destroyCalls,
			provider.destroyContextErr)
	}
}

func TestRehearsalBoundCoversCloneCreation(t *testing.T) {
	provider := freshProvider()
	options := testOptions()
	options.MaxDuration = 20 * time.Millisecond
	_, err := NewOrchestrator(slowAgeProvider{provider}, &fakeRunner{}, options).
		Rehearse(context.Background(), rehearsalPlan())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrCloneUnavailable) {
		t.Fatalf("an expired bound must stop clone creation: %v", err)
	}
	if provider.createCalls != 0 || provider.destroyCalls != 0 {
		t.Fatalf("clone lifecycle after an expired bound: create %d destroy %d",
			provider.createCalls, provider.destroyCalls)
	}
}

// slowAgeProvider answers SnapshotAge only once its context ends, so the
// bound expires before Create.
type slowAgeProvider struct{ *fakeProvider }

func (p slowAgeProvider) SnapshotAge(ctx context.Context) (time.Duration, error) {
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	return p.snapshotAge, nil
}
