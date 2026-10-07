package sre

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// A plan-regression investigation is about one statement (its subject is
// "queryid N"): its plan_regressions probe reads only that statement, so
// the probe's row cap can never hide the statement the investigation is
// about. Other families and subjects keep their plans unchanged.

func TestQuerySubject_RoundTrip(t *testing.T) {
	for _, q := range []int64{1, -1, 42, math.MaxInt64, math.MinInt64} {
		s := QuerySubject(q)
		if s != "queryid "+strconv.FormatInt(q, 10) {
			t.Fatalf("subject %q", s)
		}
		got, ok := ParseQuerySubject(s)
		if !ok || got != q {
			t.Fatalf("%q: %d %t", s, got, ok)
		}
	}
	for _, s := range []string{"", "external request", "queryid", "queryid ", "queryid 0",
		"queryid -0", "queryid abc", "queryid 1.5", "queryid 9223372036854775808",
		"queryid 12 ", " queryid 12", "queryid  12", "QUERYID 12", "queryid +12",
		"queryid 007", "pid 4242"} {
		if q, ok := ParseQuerySubject(s); ok {
			t.Errorf("%q parsed as %d", s, q)
		}
	}
}

func planArgs(steps []planStep, id probes.ID) []probes.Args {
	var out []probes.Args
	for _, st := range steps {
		for _, c := range st.calls {
			if c.id == id {
				out = append(out, c.args)
			}
		}
	}
	return out
}

func TestScopePlanQuery_ScopesThePlanProbe(t *testing.T) {
	plan, ok := planFor(TriggerPlan, time.Hour)
	if !ok {
		t.Fatal("no plan")
	}
	scoped := scopePlanQuery(plan, Investigation{TriggerKind: TriggerPlan,
		Subject: QuerySubject(-77)})
	got := planArgs(scoped, probes.PlanRegressions)
	if len(got) != 1 || got[0].QueryID != -77 {
		t.Fatalf("plan_regressions args %+v", got)
	}
	if orig := planArgs(plan, probes.PlanRegressions); len(orig) != 1 ||
		orig[0].QueryID != 0 {
		t.Fatalf("the input plan was modified: %+v", orig)
	}
	actions := planArgs(scoped, probes.SageActions)
	if len(actions) != 1 || actions[0].QueryID != 0 || actions[0].Window != time.Hour {
		t.Fatalf("other probes keep their arguments: %+v", actions)
	}
}

func TestScopePlanQuery_LeavesOtherPlansAlone(t *testing.T) {
	plan, _ := planFor(TriggerPlan, time.Hour)
	for _, inv := range []Investigation{
		{TriggerKind: TriggerPlan, Subject: "external request"},
		{TriggerKind: TriggerPlan, Subject: "queryid 0"},
		{TriggerKind: TriggerPlan, Subject: ""},
		{TriggerKind: TriggerOperator, Subject: QuerySubject(5)},
	} {
		got := planArgs(scopePlanQuery(plan, inv), probes.PlanRegressions)
		if len(got) != 1 || got[0].QueryID != 0 {
			t.Errorf("%+v: %+v", inv, got)
		}
	}
	lock, _ := planFor(TriggerLock, time.Hour)
	scoped := scopePlanQuery(lock, Investigation{TriggerKind: TriggerLock,
		Subject: QuerySubject(5)})
	for _, st := range scoped {
		for _, c := range st.calls {
			if c.args.QueryID != 0 {
				t.Fatalf("a lock plan is never query-scoped: %+v", c)
			}
		}
	}
	if scopePlanQuery(nil, Investigation{TriggerKind: TriggerPlan,
		Subject: QuerySubject(5)}) != nil {
		t.Fatal("no plan stays no plan")
	}
}

// Every scoped call must still pass the catalog's argument check.
func TestScopePlanQuery_ArgsPassTheCatalog(t *testing.T) {
	plan, _ := planFor(TriggerPlan, time.Hour)
	scoped := scopePlanQuery(plan, Investigation{TriggerKind: TriggerPlan,
		Subject: QuerySubject(123)})
	for _, st := range scoped {
		for _, c := range st.calls {
			if err := probes.Catalog().CheckArgs(c.id, c.args); err != nil {
				t.Fatalf("%s %+v: %v", c.id, c.args, err)
			}
		}
	}
}
