package changeplan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// plan-01a validation. No concurrent-access test here: Validate is a pure
// function of its plan and resolver (the store tests cover concurrency).

func validate(t *testing.T, p Plan, r Resolver) []Problem {
	t.Helper()
	ps, err := Validate(context.Background(), p, r)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return ps
}

func TestValidate_ThreeStepPlanIsClean(t *testing.T) {
	r := newFakeResolver()
	if ps := validate(t, threeStepPlan(), r); len(ps) != 0 {
		t.Fatalf("problems on a valid plan: %v", codes(ps))
	}
	if r.gotWindow != EvidenceWindow || EvidenceWindow != 14*24*time.Hour {
		t.Fatalf("calls window %v (EvidenceWindow %v), want 14 days", r.gotWindow,
			EvidenceWindow)
	}
}

// invalidCases each break one rule of threeStepPlan.
var invalidCases = []struct {
	name    string
	edit    func(*Plan, *fakeResolver)
	code    string
	ordinal int
}{
	{"cycle", func(p *Plan, _ *fakeResolver) {
		p.Edges = []Edge{{From: 1, To: 2}, {From: 2, To: 1}}
	}, CodeEdgeCycle, 1},
	{"self edge", func(p *Plan, _ *fakeResolver) {
		p.Edges = []Edge{{From: 2, To: 2}}
	}, CodeEdgeSelf, 2},
	{"edge to another plan's step", func(p *Plan, _ *fakeResolver) {
		p.Edges = []Edge{{From: 1, To: 9}}
	}, CodeEdgeForeignStep, 9},
	{"unknown action type", func(p *Plan, _ *fakeResolver) {
		p.Steps[1].ActionType = "drop_everything"
	}, CodeActionTypeUnknown, 2},
	{"refused SQL", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].ProposedSQL = "DROP TABLE public.orders"
	}, CodeSQLRefused, 1},
	{"multi-statement SQL", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].ProposedSQL += "; DROP TABLE public.orders"
	}, CodeSQLRefused, 1},
	{"missing rollback on a reversible contract", func(p *Plan, _ *fakeResolver) {
		p.Steps[2].RollbackSQL = ""
	}, CodeRollbackMissing, 3},
	{"refused rollback", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].RollbackSQL = "DROP TABLE public.orders"
	}, CodeRollbackRefused, 1},
	{"finding that does not exist", func(p *Plan, _ *fakeResolver) {
		p.Steps[1].FindingID = ptr(int64(999))
	}, CodeEvidenceUnresolved, 2},
	{"evidence id not in the pack", func(p *Plan, _ *fakeResolver) {
		p.Steps[2].EvidenceIDs = []string{"setting/work_mem"}
	}, CodeEvidenceUnresolved, 3},
	{"no finding and no evidence", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].FindingID = nil
	}, CodeEvidenceUnresolved, 1},
	{"goal of 501 characters", func(p *Plan, _ *fakeResolver) {
		p.Goal = strings.Repeat("g", 501)
	}, CodeGoalLength, 0},
	{"deadline 0", func(p *Plan, _ *fakeResolver) {
		p.Steps[1].DeadlineMinutes = 0
	}, CodeDeadlineRange, 2},
	{"deadline 43201", func(p *Plan, _ *fakeResolver) {
		p.Steps[2].DeadlineMinutes = 43201
	}, CodeDeadlineRange, 3},
	{"metric step without targets", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].TargetQueryIDs = nil
	}, CodeTargetsMissing, 1},
	{"readback without a setting", func(p *Plan, _ *fakeResolver) {
		p.Steps[2].Setting = ""
	}, CodeSettingMissing, 3},
	{"unknown kind", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].Kind = "magic"
	}, CodeKindInvalid, 1},
	{"unknown outcome action", func(p *Plan, _ *fakeResolver) {
		p.Steps[1].OnRegressed = "shrug"
	}, CodeOutcomeActionInvalid, 2},
	{"unknown metric", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].Metric = "vibes"
	}, CodeMetricInvalid, 1},
	{"duplicate ordinal", func(p *Plan, _ *fakeResolver) {
		p.Steps[1].Ordinal = 1
	}, CodeOrdinalDuplicate, 1},
	{"unknown origin", func(p *Plan, _ *fakeResolver) {
		p.Origin = "oracle"
	}, CodeOriginInvalid, 0},
	{"upper-case pack hash", func(p *Plan, _ *fakeResolver) {
		p.PackHash = strings.ToUpper(testPack)
	}, CodePackHashInvalid, 0},
	{"rollback on a no-rollback contract", func(p *Plan, _ *fakeResolver) {
		p.Steps[0].ActionType = "vacuum_table"
		p.Steps[0].ProposedSQL = "VACUUM (ANALYZE) public.orders"
	}, CodeRollbackNotApplicable, 1},
}

func TestValidate_EachInvalidInputHasItsCodeAndOrdinal(t *testing.T) {
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			p, r := threeStepPlan(), newFakeResolver()
			tc.edit(&p, r)
			ps := validate(t, p, r)
			if !hasCode(ps, tc.code, tc.ordinal) {
				t.Fatalf("problems %v, want %s@%d", codes(ps), tc.code, tc.ordinal)
			}
			for _, pr := range ps {
				if pr.Detail == "" {
					t.Errorf("problem %s@%d has no detail", pr.Code, pr.Ordinal)
				}
			}
		})
	}
}

func TestValidate_StepCountBoundaries(t *testing.T) {
	r := newFakeResolver()
	build := func(n int) Plan {
		p := threeStepPlan()
		p.Edges = nil
		p.Steps = nil
		for i := 1; i <= n; i++ {
			s := indexStep(i, "idx_"+strings.Repeat("x", i), "customer_id", 11, 101)
			p.Steps = append(p.Steps, s)
		}
		return p
	}
	for _, n := range []int{1, 50} {
		if ps := validate(t, build(n), r); len(ps) != 0 {
			t.Fatalf("%d steps: problems %v, want none", n, codes(ps))
		}
	}
	if ps := validate(t, build(51), r); !hasCode(ps, CodeStepCount, 0) {
		t.Fatalf("51 steps: problems %v, want %s@0", codes(ps), CodeStepCount)
	}
	if ps := validate(t, build(0), r); !hasCode(ps, CodeStepCount, 0) {
		t.Fatalf("no steps: problems %v, want %s@0", codes(ps), CodeStepCount)
	}
}

func TestValidate_DeadlineBoundaries(t *testing.T) {
	r := newFakeResolver()
	for minutes, ok := range map[int]bool{0: false, 1: true, 43200: true, 43201: false,
		-5: false} {
		p := threeStepPlan()
		p.Steps[0].DeadlineMinutes = minutes
		ps := validate(t, p, r)
		if got := !hasCode(ps, CodeDeadlineRange, 1); got != ok {
			t.Errorf("deadline %d: problems %v, valid=%v want %v", minutes, codes(ps), got, ok)
		}
	}
}

func TestValidate_GoalBoundaries(t *testing.T) {
	r := newFakeResolver()
	for goal, ok := range map[string]bool{"": false, "g": true,
		strings.Repeat("g", 500): true, strings.Repeat("g", 501): false,
		strings.Repeat("é", 500): true} {
		p := threeStepPlan()
		p.Goal = goal
		ps := validate(t, p, r)
		if got := !hasCode(ps, CodeGoalLength, 0); got != ok {
			t.Errorf("goal of %d runes: valid=%v want %v (%v)", len([]rune(goal)), got, ok,
				codes(ps))
		}
	}
}

// lab-01's up-front rule: a target with exactly the minimum calls is
// verifiable; one fewer must declare unverifiable (source none and an
// on_unverifiable action).
func TestValidate_UnverifiableMustBeDeclaredBelowMinCalls(t *testing.T) {
	r := newFakeResolver()
	p := threeStepPlan()
	r.calls[101] = 100 // step 1 asks for 100 calls
	if ps := validate(t, p, r); hasCode(ps, CodeUnverifiableUndeclared, 1) {
		t.Fatalf("exactly min calls: %v, want verifiable", codes(ps))
	}
	r.calls[101] = 99
	if ps := validate(t, p, r); !hasCode(ps, CodeUnverifiableUndeclared, 1) {
		t.Fatalf("one call short: %v, want %s@1", codes(ps), CodeUnverifiableUndeclared)
	}
	p.Steps[0].ExpectedSource = SourceNone
	if ps := validate(t, p, r); !hasCode(ps, CodeUnverifiableUndeclared, 1) {
		t.Fatalf("source none without on_unverifiable: %v, want undeclared", codes(ps))
	}
	p.Steps[0].OnUnverifiable = ActionKeep
	p.Steps[0].ExpectedPct = nil
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("declared unverifiable: %v, want clean", codes(ps))
	}
	r.calls[101] = 0 // no rollup rows at all: fail closed, still undeclared
	p.Steps[0].OnUnverifiable = ""
	if ps := validate(t, p, r); !hasCode(ps, CodeUnverifiableUndeclared, 1) {
		t.Fatalf("no calls recorded: %v, want undeclared", codes(ps))
	}
}

func TestValidate_DefaultMinCallsApplyWhenUnset(t *testing.T) {
	r := newFakeResolver()
	p := threeStepPlan()
	p.Steps[0].MinEvidence = MinEvidence{}
	r.calls[101] = DefaultMinCalls - 1
	if ps := validate(t, p, r); !hasCode(ps, CodeUnverifiableUndeclared, 1) {
		t.Fatalf("below the default %d calls: %v, want undeclared", DefaultMinCalls,
			codes(ps))
	}
	r.calls[101] = DefaultMinCalls
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("at the default calls: %v, want clean", codes(ps))
	}
}

func TestValidate_NilAndEmptyInputs(t *testing.T) {
	r := newFakeResolver()
	p := threeStepPlan()
	p.Scope = Scope{}
	p.Edges = nil
	p.PackHash = ""
	p.Steps[2].EvidenceIDs = nil
	p.Steps[2].FindingID = ptr(int64(13))
	p.Steps[0].Guardrails = nil
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("nil scope, edges, pack and evidence: %v, want clean", codes(ps))
	}
	if _, err := Validate(context.Background(), p, nil); err == nil ||
		!strings.Contains(err.Error(), "resolver") {
		t.Fatalf("nil resolver: %v, want an error naming the resolver", err)
	}
	// Evidence ids without a pack cannot resolve (no pack to look in).
	p.Steps[2].FindingID = nil
	p.Steps[2].EvidenceIDs = []string{"setting/random_page_cost"}
	if ps := validate(t, p, r); !hasCode(ps, CodeEvidenceUnresolved, 3) {
		t.Fatalf("evidence ids with no pack: %v, want unresolved", codes(ps))
	}
}

func TestValidate_ResolverErrorsAreErrorsNotProblems(t *testing.T) {
	cause := errors.New("connection refused")
	for name, set := range map[string]func(*fakeResolver){
		"finding":  func(r *fakeResolver) { r.findErr = cause },
		"evidence": func(r *fakeResolver) { r.evErr = cause },
		"calls":    func(r *fakeResolver) { r.callsErr = cause },
	} {
		t.Run(name, func(t *testing.T) {
			r := newFakeResolver()
			set(r)
			p := threeStepPlan()
			if name == "evidence" {
				p.Steps[0].EvidenceIDs = []string{"setting/random_page_cost"}
			}
			ps, err := Validate(context.Background(), p, r)
			if err == nil || !errors.Is(err, cause) {
				t.Fatalf("err %v, want it to wrap the cause", err)
			}
			if !strings.Contains(err.Error(), "resolve evidence for step ") {
				t.Fatalf("err %q, want it to name the step", err)
			}
			if hasCode(ps, CodeEvidenceUnresolved, 1) || hasCode(ps, CodeEvidenceUnresolved, 3) {
				t.Fatalf("a resolver failure was reported as unresolved evidence: %v", codes(ps))
			}
		})
	}
}

func TestValidate_OperatorAndCheckSteps(t *testing.T) {
	r := newFakeResolver()
	p := threeStepPlan()
	p.Steps[2] = Step{Ordinal: 3, Kind: KindOperator,
		OperatorCommand: "Set work_mem to 64MB in the provider console",
		Landing:         &Landing{Setting: "work_mem", Value: "64MB"}, FindingID: ptr(int64(13)),
		Setting: "work_mem", Metric: MetricReadback, ExpectedDirection: DirectionNone,
		ExpectedSource: SourceNone, DeadlineMinutes: 1440}
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("operator step: %v, want clean", codes(ps))
	}
	p.Steps[2].OperatorCommand = ""
	if ps := validate(t, p, r); !hasCode(ps, CodeCommandMissing, 3) {
		t.Fatalf("operator step with nothing to run: %v, want %s@3", codes(ps),
			CodeCommandMissing)
	}
	p.Steps[2] = Step{Ordinal: 3, Kind: KindCheck, Setting: "random_page_cost",
		Metric: MetricReadback, ExpectedDirection: DirectionNone, ExpectedSource: SourceNone,
		DeadlineMinutes: 10, RollbackSQL: "ALTER SYSTEM RESET random_page_cost"}
	if ps := validate(t, p, r); !hasCode(ps, CodeRollbackNotApplicable, 3) {
		t.Fatalf("check step with a rollback: %v, want %s@3", codes(ps),
			CodeRollbackNotApplicable)
	}
}

func TestValidate_EmptyEdgesStillValidOverManySteps(t *testing.T) {
	p := threeStepPlan()
	p.Edges = []Edge{}
	if ps := validate(t, p, newFakeResolver()); len(ps) != 0 {
		t.Fatalf("empty edge list: %v, want clean (ordinal order)", codes(ps))
	}
	p.Edges = []Edge{{From: 1, To: 3}, {From: 1, To: 3}}
	if ps := validate(t, p, newFakeResolver()); !hasCode(ps, CodeEdgeDuplicate, 1) {
		t.Fatalf("duplicate edge: %v, want %s@1", codes(ps), CodeEdgeDuplicate)
	}
	// A longer cycle through three steps is still a cycle.
	p.Edges = []Edge{{From: 1, To: 2}, {From: 2, To: 3}, {From: 3, To: 1}}
	ps := validate(t, p, newFakeResolver())
	found := false
	for _, pr := range ps {
		found = found || pr.Code == CodeEdgeCycle
	}
	if !found {
		t.Fatalf("1->2->3->1: %v, want a cycle", codes(ps))
	}
}

func TestStep_ActionForDefaultsAndFailsClosed(t *testing.T) {
	s := Step{}
	want := map[Outcome]OutcomeAction{OutcomeImproved: ActionKeep,
		OutcomeNoChange: ActionKeep, OutcomeRegressed: ActionRevertStep,
		OutcomeInconclusive: ActionHaltAsk, OutcomeUnverifiable: ActionHaltAsk,
		OutcomeExpired: ActionHaltAsk, "made_up": ActionHaltAsk}
	for o, a := range want {
		if got := s.ActionFor(o); got != a {
			t.Errorf("default for %s = %s, want %s", o, got, a)
		}
	}
	s.OnUnverifiable = ActionKeep
	if got := s.ActionFor(OutcomeUnverifiable); got != ActionKeep {
		t.Fatalf("declared unverifiable keep = %s", got)
	}
	d := Step{}.WithDefaults()
	if d.OnUnverifiable != "" || d.OnRegressed != ActionRevertStep ||
		d.MinEvidence.MinCalls != DefaultMinCalls || d.Runtime.State != StatePending {
		t.Fatalf("WithDefaults = %+v", d)
	}
	for in, out := range map[string]Outcome{"improved": OutcomeImproved,
		"neutral": OutcomeNoChange, "regressed": OutcomeRegressed,
		"insufficient_evidence": OutcomeInconclusive, "unverifiable": OutcomeUnverifiable,
		"expired": OutcomeExpired, "": OutcomeInconclusive, "garbage": OutcomeInconclusive} {
		if got := MapVerifierOutcome(in); got != out {
			t.Errorf("MapVerifierOutcome(%q) = %s, want %s", in, got, out)
		}
	}
}
