package changeplan

import (
	"context"
	"reflect"
	"testing"
)

// plan-01a review: a check step changes nothing, so it carries no SQL.
// The executor's allowlist accepts only changes (DDL, ALTER SYSTEM, a
// backend signal), so a check step's SQL would be a change that skipped
// the contract, citation and rollback rules. No concurrency test here:
// Validate is a pure function of its plan and resolver.

func checkStep(ordinal int, sql string) Step {
	return Step{Ordinal: ordinal, Kind: KindCheck, Setting: "random_page_cost",
		ProposedSQL: sql, Metric: MetricReadback, ExpectedDirection: DirectionNone,
		ExpectedSource: SourceNone, DeadlineMinutes: 10}
}

func TestValidate_CheckStepCarriesNoSQL(t *testing.T) {
	r := newFakeResolver()
	p := threeStepPlan()
	p.Steps[2] = checkStep(3, "")
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("check step without SQL: %v, want clean", codes(ps))
	}
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY idx_c ON public.orders (status)",
		"DROP INDEX CONCURRENTLY public.idx_a",
		"ALTER SYSTEM SET random_page_cost = 4",
		"SELECT pg_terminate_backend(4242)",
		"SELECT current_setting('random_page_cost')",
	} {
		p.Steps[2] = checkStep(3, sql)
		ps := validate(t, p, r)
		if !hasCode(ps, CodeSQLNotApplicable, 3) || len(ps) != 1 {
			t.Errorf("check step with %q: %v, want only %s@3", sql, codes(ps),
				CodeSQLNotApplicable)
		}
	}
}

func TestStore_CreateLeavesTheCallersStepsAlone(t *testing.T) {
	s := NewStore(planPool(t, "plan_caller_steps"))
	in := threeStepPlan()
	in.Steps[0].Runtime = StepRuntime{State: StateKept, StateReason: "from the caller"}
	before := make([]Step, len(in.Steps))
	copy(before, in.Steps)
	got, err := s.Create(context.Background(), in, newFakeResolver())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !reflect.DeepEqual(in.Steps, before) {
		t.Fatalf("Create changed the caller's steps:\n got %+v\nwant %+v", in.Steps, before)
	}
	if got.Steps[0].Runtime.State != StatePending || got.Steps[0].Runtime.StateReason != "" ||
		got.Steps[0].OnRegressed != ActionRevertStep {
		t.Fatalf("stored step 1 %+v, want pending with the defaults applied", got.Steps[0])
	}
}
