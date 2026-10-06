package sre

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The coordinator runs a plan-regression investigation's plan_regressions
// probe scoped to the investigation's statement; an investigation that is
// not about one statement runs it unscoped.

type argsRunner struct {
	*scriptedRunner
	mu   sync.Mutex
	args map[probes.ID][]probes.Args
}

func (r *argsRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	r.mu.Lock()
	r.args[id] = append(r.args[id], a)
	r.mu.Unlock()
	return r.scriptedRunner.Run(ctx, id, a)
}

func (r *argsRunner) argsOf(id probes.ID) []probes.Args {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]probes.Args(nil), r.args[id]...)
}

func TestCoordinator_PlanProbeIsScopedToTheStatement(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	flip := func(q int64, before, after float64) probes.Row {
		return probes.Row{"queryid": q, "previous_plan_hash": "v1:a",
			"current_plan_hash": "v1:b", "plan_flipped": true,
			"flipped_at":   time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC),
			"before_calls": int64(20), "before_mean_ms": before,
			"after_calls": int64(20), "after_mean_ms": after}
	}
	runner := &argsRunner{scriptedRunner: newScriptedRunner().script(probes.PlanRegressions,
		rows(probes.PlanRegressions, flip(-2, 1, 50))), args: map[probes.ID][]probes.Args{}}
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "specialist:plan-q", Kind: TriggerPlan,
		Subject: QuerySubject(-2), IdempotencyKey: "spec:plan-q"})
	if inv.State != StateConcluded || inv.Summary.Subject != "queryid -2" {
		t.Fatalf("plan investigation = %s %+v", inv.State, inv.Summary)
	}
	got := runner.argsOf(probes.PlanRegressions)
	if len(got) != 1 || got[0].QueryID != -2 {
		t.Fatalf("plan_regressions args %+v, want queryid -2", got)
	}
	unscoped := startAndRun(t, ctx, c, Trigger{CaseID: "specialist:plan-x",
		Kind: TriggerPlan, Subject: "external request", IdempotencyKey: "spec:plan-x"})
	got = runner.argsOf(probes.PlanRegressions)
	if len(got) != 2 || got[1].QueryID != 0 || unscoped.State != StateInconclusive {
		t.Fatalf("an investigation without a statement runs unscoped: %+v %s", got,
			unscoped.State)
	}
}
