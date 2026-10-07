package probes

import (
	"errors"
	"math"
	"testing"
	"time"
)

// Query-scoped probes (specialist contract revision 1.1.0): a probe whose
// spec is QueryScoped takes an optional statement identity (queryid) and
// binds it as the parameter after its typed arguments (0 reads every
// statement). No other probe accepts a queryid.

func TestQueryScoped_OnlyPlanRegressions(t *testing.T) {
	reg := Catalog()
	for _, id := range reg.IDs() {
		spec, _ := reg.Spec(id)
		if spec.QueryScoped != (id == PlanRegressions) {
			t.Errorf("%s: QueryScoped=%t", id, spec.QueryScoped)
		}
	}
}

func TestCheckArgs_QueryID(t *testing.T) {
	reg := Catalog()
	for _, a := range []Args{{QueryID: 1}, {QueryID: -1}, {QueryID: math.MaxInt64},
		{QueryID: math.MinInt64}, {QueryID: 7, Window: time.Hour}} {
		if err := reg.CheckArgs(PlanRegressions, a); err != nil {
			t.Errorf("plan_regressions %+v: %v", a, err)
		}
	}
	bad := []struct {
		id   ID
		args Args
	}{
		{LockGraph, Args{QueryID: 1}},
		{SageActions, Args{QueryID: 1}},
		{SageActions, Args{QueryID: 1, Window: time.Hour}},
		{BackendIdentity, Args{QueryID: 1, PID: 5, BackendStart: time.Now()}},
		{SequenceRunwayProbe, Args{QueryID: 1}},
		{PlanRegressions, Args{QueryID: 1, PID: 5}},
		{PlanRegressions, Args{QueryID: 1, Window: MaxWindow + time.Second}},
	}
	for _, c := range bad {
		if err := reg.CheckArgs(c.id, c.args); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s %+v: %v, want ErrInvalidArgs", c.id, c.args, err)
		}
	}
}

func TestBind_AppendsTheQueryIDOnlyForQueryScopedProbes(t *testing.T) {
	plan, _ := Catalog().Spec(PlanRegressions)
	got := Args{QueryID: -5, Window: time.Hour}.bind(plan, 21)
	if len(got) != 3 || got[0] != 21 || got[1] != time.Hour.Seconds() ||
		got[2] != int64(-5) {
		t.Fatalf("plan_regressions params %#v", got)
	}
	got = Args{}.bind(plan, 21)
	if len(got) != 3 || got[2] != int64(0) || got[1] != DefaultWindow.Seconds() {
		t.Fatalf("unscoped plan_regressions params %#v", got)
	}
	actions, _ := Catalog().Spec(SageActions)
	if got := (Args{Window: time.Hour}).bind(actions, 21); len(got) != 2 {
		t.Fatalf("a window probe binds limit and window only: %#v", got)
	}
}
