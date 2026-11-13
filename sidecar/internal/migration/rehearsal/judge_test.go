package rehearsal

import (
	"context"
	"strings"
	"testing"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
)

// No concurrent access tests: the budget check is a pure function of
// one measurement and the options (migration-06 part A).

// An explicit PlanFlipPct replaces the default.
func TestRegressedHonoursConfiguredPlanFlipPct(t *testing.T) {
	options := testOptions()
	options.PlanFlipPct = 10
	measurement := Measurement{AffectedQueries: []QueryMeasurement{{
		QueryID: 7, BeforeLatencyMS: 100, AfterLatencyMS: 108,
		BeforePlanHash: "plan-a", AfterPlanHash: "plan-b",
	}}}
	if got := NewOrchestrator(nil, nil, options).judge(measurement); got.Verdict !=
		VerdictPromoteExpand {
		t.Fatalf("+8%% flip with plan_flip_pct 10: %#v", got)
	}
	options.PlanFlipPct = 7.5
	if got := NewOrchestrator(nil, nil, options).judge(measurement); got.Verdict !=
		VerdictPark {
		t.Fatalf("+8%% flip with plan_flip_pct 7.5: %#v", got)
	}
}

// Error propagation: a 7-minute step against the 10-minute statement
// timeout parks (7m x 1.5 = 10m30s), and the detail names the step and
// the measured value. It parks even without workload evidence.
func TestJudgeParksStepOverStatementBudget(t *testing.T) {
	options := testOptions()
	options.StatementTimeout = 10 * time.Minute
	measurement := Measurement{Steps: []StepMeasurement{{
		Kind: planpkg.StepValidateConstraint, Duration: 7 * time.Minute,
		LockLevel: "SHARE UPDATE EXCLUSIVE",
	}}}
	got := NewOrchestrator(nil, nil, options).judge(measurement)
	if got.Verdict != VerdictPark || got.Reason != ReasonStepBudget {
		t.Fatalf("result = %#v", got)
	}
	for _, want := range []string{"validate_constraint", "7m0s", "10m0s"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("detail %q does not name %q", got.Detail, want)
		}
	}
	if got.Measurement.Steps[0].Duration != 7*time.Minute {
		t.Fatalf("measurement not preserved: %#v", got.Measurement)
	}
}

// Boundary: 6m40s x 1.5 is exactly 10m and does not park; one second
// more does.
func TestJudgeStatementBudgetBoundary(t *testing.T) {
	options := testOptions()
	options.StatementTimeout = 10 * time.Minute
	for _, test := range []struct {
		duration time.Duration
		park     bool
	}{
		{400 * time.Second, false}, {401 * time.Second, true},
	} {
		measurement := passingMeasurement()
		measurement.Steps = []StepMeasurement{{Kind: planpkg.StepValidateConstraint,
			Duration: test.duration, LockLevel: "SHARE UPDATE EXCLUSIVE"}}
		got := NewOrchestrator(nil, nil, options).judge(measurement)
		if (got.Reason == ReasonStepBudget) != test.park {
			t.Fatalf("step %s: result = %#v, park=%v", test.duration, got, test.park)
		}
	}
}

// Zero options fall back to the 10-minute statement timeout (fail closed:
// an unset budget is not an unlimited one).
func TestJudgeZeroStatementTimeoutUsesDefault(t *testing.T) {
	measurement := Measurement{Steps: []StepMeasurement{{
		Kind: planpkg.StepCreateUniqueIndex, Duration: 7 * time.Minute,
		Concurrent: true, LockLevel: "SHARE UPDATE EXCLUSIVE",
	}}}
	got := NewOrchestrator(nil, nil, Options{}).judge(measurement)
	if got.Verdict != VerdictPark || got.Reason != ReasonStepBudget {
		t.Fatalf("result = %#v", got)
	}
}

// A non-CONCURRENTLY step holding ACCESS EXCLUSIVE past max_lock parks;
// a CONCURRENTLY step and a weaker lock do not.
func TestJudgeParksLongAccessExclusiveStep(t *testing.T) {
	options := testOptions()
	options.MaxLock = 2 * time.Second
	tests := []struct {
		name string
		step StepMeasurement
		park bool
	}{
		{"access exclusive over budget", StepMeasurement{Kind: planpkg.StepAddCheckNotValid,
			Duration: 2001 * time.Millisecond, LockLevel: "ACCESS EXCLUSIVE"}, true},
		{"access exclusive at budget", StepMeasurement{Kind: planpkg.StepAddCheckNotValid,
			Duration: 2 * time.Second, LockLevel: "ACCESS EXCLUSIVE"}, false},
		{"unknown lock is treated as access exclusive", StepMeasurement{
			Kind: planpkg.StepAddCheckNotValid, Duration: 3 * time.Second}, true},
		{"concurrent step is exempt", StepMeasurement{Kind: planpkg.StepCreateUniqueIndex,
			Duration: time.Minute, Concurrent: true, LockLevel: "ACCESS EXCLUSIVE"}, false},
		{"weaker lock is exempt", StepMeasurement{Kind: planpkg.StepValidateConstraint,
			Duration: time.Minute, LockLevel: "SHARE UPDATE EXCLUSIVE"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			measurement := passingMeasurement()
			measurement.Steps = []StepMeasurement{test.step}
			got := NewOrchestrator(nil, nil, options).judge(measurement)
			if (got.Reason == ReasonStepBudget) != test.park {
				t.Fatalf("result = %#v, want park=%v", got, test.park)
			}
			if test.park && (got.Verdict != VerdictPark ||
				!strings.Contains(got.Detail, "max_lock_ms")) {
				t.Fatalf("park result = %#v", got)
			}
		})
	}
}

// State transition: the budget park reaches Rehearse's result.
func TestRehearseParksOnStepBudget(t *testing.T) {
	provider := freshProvider()
	measurement := passingMeasurement()
	measurement.Steps = []StepMeasurement{{Kind: planpkg.StepAddCheckNotValid,
		Duration: 5 * time.Second, LockLevel: "ACCESS EXCLUSIVE"}}
	runner := &fakeRunner{measurement: measurement}
	got, err := NewOrchestrator(provider, runner, testOptions()).Rehearse(
		context.Background(), rehearsalPlan())
	if err != nil {
		t.Fatalf("Rehearse: %v", err)
	}
	if got.Verdict != VerdictPark || got.Reason != ReasonStepBudget ||
		provider.destroyCalls != 1 {
		t.Fatalf("result = %#v destroy=%d", got, provider.destroyCalls)
	}
}
