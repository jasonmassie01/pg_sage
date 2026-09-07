package rehearsal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
)

func TestOrchestratorStaleCloneDowngradesToRecommendOnly(t *testing.T) {
	provider := &fakeProvider{snapshotAge: 25 * time.Hour}
	runner := &fakeRunner{}
	orchestrator := NewOrchestrator(provider, runner, Options{
		MaxCloneAge: 24 * time.Hour, RegressionPct: 15,
	})

	result, err := orchestrator.Rehearse(context.Background(), rehearsalPlan())

	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if result.Verdict != VerdictRecommendOnly || result.Reason != ReasonStaleClone {
		t.Fatalf("Result = %#v", result)
	}
	if provider.createCalls != 0 || provider.destroyCalls != 0 || runner.calls != 0 {
		t.Fatalf("stale clone touched runtime: provider=%#v runner=%#v", provider, runner)
	}
}

func TestOrchestratorAlwaysDestroysClone(t *testing.T) {
	applyErr := errors.New("migration apply failed")
	regressionErr := errors.New("measure plans failed")
	tests := []struct {
		name   string
		runner *fakeRunner
	}{
		{"success", &fakeRunner{measurement: passingMeasurement()}},
		{"apply failure", &fakeRunner{err: applyErr}},
		{"measurement failure", &fakeRunner{err: regressionErr}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := freshProvider()
			orchestrator := NewOrchestrator(provider, test.runner, testOptions())

			_, _ = orchestrator.Rehearse(context.Background(), rehearsalPlan())

			if provider.createCalls != 1 || provider.destroyCalls != 1 {
				t.Fatalf("clone lifecycle = create %d destroy %d",
					provider.createCalls, provider.destroyCalls)
			}
			if provider.destroyed.ID != "clone-1" {
				t.Fatalf("destroyed clone = %#v", provider.destroyed)
			}
			if provider.destroyContextErr != nil {
				t.Fatalf("destroy context was already cancelled: %v",
					provider.destroyContextErr)
			}
		})
	}
}

func TestOrchestratorParksPlanRegressionAndPreservesEvidence(t *testing.T) {
	provider := freshProvider()
	runner := &fakeRunner{measurement: Measurement{
		MaxLockDuration:  80 * time.Millisecond,
		BackfillDuration: 2 * time.Minute,
		DiskDeltaBytes:   512 << 20,
		AffectedQueries: []QueryMeasurement{{
			QueryID: 42, BeforeLatencyMS: 10, AfterLatencyMS: 13,
			BeforePlanHash: "plan-a", AfterPlanHash: "plan-b",
		}},
	}}
	orchestrator := NewOrchestrator(provider, runner, testOptions())

	result, err := orchestrator.Rehearse(context.Background(), rehearsalPlan())

	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if result.Verdict != VerdictPark || result.Reason != ReasonPlanRegression {
		t.Fatalf("Result = %#v", result)
	}
	if result.Measurement.AffectedQueries[0].QueryID != 42 ||
		result.Measurement.MaxLockDuration != 80*time.Millisecond {
		t.Fatalf("measurement evidence = %#v", result.Measurement)
	}
	if provider.destroyCalls != 1 {
		t.Fatalf("destroy calls = %d, want 1", provider.destroyCalls)
	}
}

func TestOrchestratorNeverRehearsesContractInExpandCycle(t *testing.T) {
	provider := freshProvider()
	runner := &fakeRunner{measurement: passingMeasurement()}
	plan := rehearsalPlan()
	plan.ContractSteps = []planpkg.Step{{
		Kind: planpkg.StepSetNotNull, Phase: planpkg.PhaseContract,
		SQL: `ALTER TABLE "public"."accounts" ALTER COLUMN "email" SET NOT NULL`,
	}}
	plan.ContractNotBeforeCycle = plan.Cycle + 1

	result, err := NewOrchestrator(provider, runner, testOptions()).Rehearse(
		context.Background(), plan,
	)
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if result.Verdict != VerdictPromoteExpand {
		t.Fatalf("Result = %#v", result)
	}
	if len(runner.received.ContractSteps) != 0 {
		t.Fatalf("runner received same-cycle contract steps %#v",
			runner.received.ContractSteps)
	}
	if !reflect.DeepEqual(runner.received.ExpandSteps, plan.ExpandSteps) {
		t.Fatalf("runner expand steps = %#v, want %#v",
			runner.received.ExpandSteps, plan.ExpandSteps)
	}
}

func TestOrchestratorCancellationStillDestroysClone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{run: func(ctx context.Context, _ planpkg.Plan) error {
		cancel()
		return ctx.Err()
	}}
	provider := freshProvider()
	orchestrator := NewOrchestrator(provider, runner, testOptions())

	result, err := orchestrator.Rehearse(ctx, rehearsalPlan())

	if !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "rehearse migration") {
		t.Fatalf("Rehearse error = %v", err)
	}
	if result.Verdict != VerdictRecommendOnly || provider.destroyCalls != 1 {
		t.Fatalf("cancel result/lifecycle = %#v destroy=%d",
			result, provider.destroyCalls)
	}
	if provider.destroyContextErr != nil {
		t.Fatalf("cleanup inherited cancelled context: %v", provider.destroyContextErr)
	}
}

type fakeProvider struct {
	snapshotAge       time.Duration
	snapshotErr       error
	clone             clonepkg.Clone
	createErr         error
	destroyErr        error
	createCalls       int
	destroyCalls      int
	destroyed         clonepkg.Clone
	destroyContextErr error
}

func (provider *fakeProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return provider.snapshotAge, provider.snapshotErr
}

func (provider *fakeProvider) Create(
	ctx context.Context, _ clonepkg.CloneSpec,
) (clonepkg.Clone, error) {
	if err := ctx.Err(); err != nil {
		return clonepkg.Clone{}, err
	}
	provider.createCalls++
	return provider.clone, provider.createErr
}

func (provider *fakeProvider) Destroy(
	ctx context.Context, target clonepkg.Clone,
) error {
	provider.destroyCalls++
	provider.destroyed = target
	provider.destroyContextErr = ctx.Err()
	return provider.destroyErr
}

type fakeRunner struct {
	measurement Measurement
	err         error
	run         func(context.Context, planpkg.Plan) error
	calls       int
	received    planpkg.Plan
}

func (runner *fakeRunner) Run(
	ctx context.Context, _ string, plan planpkg.Plan,
) (Measurement, error) {
	runner.calls++
	runner.received = plan
	if runner.run != nil {
		if err := runner.run(ctx, plan); err != nil {
			return Measurement{}, err
		}
	}
	return runner.measurement, runner.err
}

func freshProvider() *fakeProvider {
	return &fakeProvider{
		snapshotAge: time.Hour,
		clone: clonepkg.Clone{
			ID: "clone-1", DSN: "postgres://clone.invalid/app",
			CreatedFrom: time.Now().Add(-time.Hour),
		},
	}
}

func testOptions() Options {
	return Options{MaxCloneAge: 24 * time.Hour, RegressionPct: 15}
}

func passingMeasurement() Measurement {
	return Measurement{
		MaxLockDuration: 20 * time.Millisecond,
		AffectedQueries: []QueryMeasurement{{
			QueryID: 42, BeforeLatencyMS: 10, AfterLatencyMS: 10.5,
			BeforePlanHash: "plan-a", AfterPlanHash: "plan-a",
		}},
	}
}

func rehearsalPlan() planpkg.Plan {
	return planpkg.Plan{
		Cycle: 7, RequiresRehearsal: true,
		ExpandSteps: []planpkg.Step{{
			Kind: planpkg.StepCreateUniqueIndex, Phase: planpkg.PhaseExpand,
			SQL:              "CREATE UNIQUE INDEX CONCURRENTLY idx ON public.users (email)",
			RequiresTopLevel: true,
		}},
	}
}

var _ clonepkg.Provider = (*fakeProvider)(nil)
var _ Runner = (*fakeRunner)(nil)
