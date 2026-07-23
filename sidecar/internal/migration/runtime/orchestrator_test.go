package runtime

import (
	"context"
	"errors"
	"testing"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/policy"
)

func TestApplyRehearsesThenAuthorizesEveryExpandStep(t *testing.T) {
	fixture := newFixture()
	result, err := fixture.orchestrator().Apply(context.Background(), Request{
		SQL: addUniqueSQL, Cycle: 4,
		Table: planpkg.TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictExpanded || fixture.applier.calls != 1 {
		t.Fatalf("result=%#v apply calls=%d", result, fixture.applier.calls)
	}
	if fixture.gate.calls != 1 || fixture.gate.requests[0].Feature != "online_migration" {
		t.Fatalf("gate calls=%d requests=%#v", fixture.gate.calls, fixture.gate.requests)
	}
	if fixture.events.String() != "plan,rehearse,authorize,apply,record" {
		t.Fatalf("event order=%q", fixture.events.String())
	}
}

func TestApplyNeverRunsContractInExpandCycle(t *testing.T) {
	fixture := newFixture()
	result, err := fixture.orchestrator().Apply(context.Background(), Request{
		SQL: addUniqueSQL, Cycle: 7,
		Table: planpkg.TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.ContractNotBeforeCycle != 8 {
		t.Fatalf("contract cycle=%d, want 8", result.ContractNotBeforeCycle)
	}
	for _, step := range fixture.applier.steps {
		if step.Phase == planpkg.PhaseContract || step.Destructive {
			t.Fatalf("contract/destructive step ran in expand cycle: %#v", step)
		}
	}
}

func TestApplyStaleCloneDowngradesWithoutAuthorizationOrMutation(t *testing.T) {
	fixture := newFixture()
	fixture.rehearser.result = rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictRecommendOnly,
		Reason:  rehearsalpkg.ReasonStaleClone,
	}
	result, err := fixture.orchestrator().Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictRecommendOnly || fixture.gate.calls != 0 ||
		fixture.applier.calls != 0 {
		t.Fatalf("result=%#v gate=%d apply=%d", result, fixture.gate.calls,
			fixture.applier.calls)
	}
}

func TestApplyPlanRegressionParksWithoutMutation(t *testing.T) {
	fixture := newFixture()
	fixture.rehearser.result = rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictPark,
		Reason:  rehearsalpkg.ReasonPlanRegression,
	}
	result, err := fixture.orchestrator().Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictParked || fixture.applier.calls != 0 {
		t.Fatalf("result=%#v apply=%d", result, fixture.applier.calls)
	}
}

func TestApplyPolicyDenialIsDurablyParked(t *testing.T) {
	fixture := newFixture()
	fixture.gate.decision = policy.Decision{
		Verdict: policy.VerdictPark, Reason: policy.ReasonOutsideMaintenanceWindow,
		EvidenceID: "ev-parked",
	}
	result, err := fixture.orchestrator().Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictParked || fixture.applier.calls != 0 {
		t.Fatalf("result=%#v apply=%d", result, fixture.applier.calls)
	}
	if fixture.recorder.last.Verdict != VerdictParked ||
		fixture.recorder.last.EvidenceID != "ev-parked" {
		t.Fatalf("recorded=%#v", fixture.recorder.last)
	}
}

func TestApplyErrorsBeforeMutationAreRecorded(t *testing.T) {
	fixture := newFixture()
	fixture.rehearser.err = errors.New("clone create failed")
	_, err := fixture.orchestrator().Apply(context.Background(), request())
	if err == nil || fixture.applier.calls != 0 {
		t.Fatalf("err=%v apply=%d", err, fixture.applier.calls)
	}
	if fixture.recorder.last.Verdict != VerdictFailed {
		t.Fatalf("recorded verdict=%q", fixture.recorder.last.Verdict)
	}
}

const addUniqueSQL = `ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)`

func request() Request {
	return Request{SQL: addUniqueSQL, Cycle: 1,
		Table: planpkg.TableFacts{Schema: "public", Name: "users"}}
}

type fixture struct {
	planner   *fakePlanner
	rehearser *fakeRehearser
	gate      *fakeGate
	applier   *fakeApplier
	recorder  *fakeRecorder
	events    *eventLog
}

func newFixture() *fixture {
	events := &eventLog{}
	return &fixture{
		planner: &fakePlanner{events: events},
		rehearser: &fakeRehearser{events: events, result: rehearsalpkg.Result{
			Verdict: rehearsalpkg.VerdictPromoteExpand,
			Reason:  rehearsalpkg.ReasonRehearsalPassed,
		}},
		gate: &fakeGate{events: events, decision: policy.Decision{
			Verdict: policy.VerdictExecute, EvidenceID: "ev-execute",
		}},
		applier:  &fakeApplier{events: events},
		recorder: &fakeRecorder{events: events}, events: events,
	}
}

func (f *fixture) orchestrator() *Orchestrator {
	return NewOrchestrator(f.planner, f.rehearser, f.gate, f.applier, f.recorder)
}

type eventLog struct{ values []string }

func (e *eventLog) add(value string) { e.values = append(e.values, value) }
func (e *eventLog) String() string {
	result := ""
	for index, value := range e.values {
		if index > 0 {
			result += ","
		}
		result += value
	}
	return result
}

type fakePlanner struct{ events *eventLog }

func (p *fakePlanner) Plan(ctx context.Context, req planpkg.Request) (planpkg.Plan, error) {
	p.events.add("plan")
	return planpkg.NewPlanner().Plan(ctx, req)
}

type fakeRehearser struct {
	result rehearsalpkg.Result
	err    error
	events *eventLog
}

func (r *fakeRehearser) Rehearse(context.Context, planpkg.Plan) (rehearsalpkg.Result, error) {
	r.events.add("rehearse")
	return r.result, r.err
}

type fakeGate struct {
	decision policy.Decision
	calls    int
	requests []policy.ActionRequest
	events   *eventLog
}

func (g *fakeGate) Authorize(_ context.Context, req policy.ActionRequest) policy.Decision {
	g.events.add("authorize")
	g.calls++
	g.requests = append(g.requests, req)
	return g.decision
}

type fakeApplier struct {
	calls  int
	steps  []planpkg.Step
	events *eventLog
}

func (a *fakeApplier) Apply(_ context.Context, step planpkg.Step) error {
	a.events.add("apply")
	a.calls++
	a.steps = append(a.steps, step)
	return nil
}

type fakeRecorder struct {
	last   Record
	events *eventLog
}

func (r *fakeRecorder) Record(_ context.Context, record Record) error {
	r.events.add("record")
	r.last = record
	return nil
}
