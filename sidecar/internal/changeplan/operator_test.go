package changeplan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// plan-02d: operator-applied steps ("you run this"). pg_sage changes
// nothing in them: it says what to run, waits for the change to land,
// judges it and never reverts it. No concurrent-access test here: these
// are pure functions; the engine's claim (plan-02a) is the only writer of
// a step, and operator_db_test.go runs the engine on a real database.

// operatorStep is a free-text operator step that lands when work_mem
// reads back as 64MB.
func operatorStep(ordinal int) Step {
	return Step{
		Ordinal:           ordinal,
		Kind:              KindOperator,
		OperatorCommand:   "aws rds modify-db-parameter-group --parameters work_mem=65536",
		OperatorRevert:    "aws rds reset-db-parameter-group --parameters work_mem",
		Landing:           &Landing{Setting: "work_mem", Value: "64MB"},
		Setting:           "work_mem",
		FindingID:         ptr(int64(13)),
		Metric:            MetricReadback,
		ExpectedDirection: DirectionNone,
		ExpectedSource:    SourceNone,
		DeadlineMinutes:   60,
	}
}

func operatorPlan() Plan {
	p := threeStepPlan()
	p.Steps = []Step{operatorStep(1)}
	p.Edges = nil
	return p
}

func TestValidate_OperatorStepHappyPath(t *testing.T) {
	if ps := validate(t, operatorPlan(), newFakeResolver()); len(ps) != 0 {
		t.Fatalf("free-text operator step: %v, want clean", codes(ps))
	}
	r := newFakeResolver()
	r.managed[42] = true
	p := operatorPlan()
	p.Steps[0].Landing, p.Steps[0].ManagedChangeID = nil, ptr(int64(42))
	if ps := validate(t, p, r); len(ps) != 0 {
		t.Fatalf("operator step landing by its managed change: %v, want clean", codes(ps))
	}
}

// Invalid input: a revert of any kind on any outcome, a landing on a
// setting the server does not have, a malformed landing, SQL for pg_sage
// to run.
func TestValidate_OperatorStepRefusals(t *testing.T) {
	cases := map[string]struct {
		edit func(*Step)
		code string
	}{
		"revert_step on regressed": {func(s *Step) { s.OnRegressed = ActionRevertStep },
			CodeOperatorNotRevertible},
		"revert_to_checkpoint on no change": {func(s *Step) {
			s.OnNoChange = ActionRevertToCheckpoint
		}, CodeOperatorNotRevertible},
		"revert_step on expired": {func(s *Step) { s.OnExpired = ActionRevertStep },
			CodeOperatorNotRevertible},
		"unknown setting": {func(s *Step) { s.Landing.Setting = "work_memory" },
			CodeLandingInvalid},
		"malformed setting": {func(s *Step) { s.Landing.Setting = "work_mem; DROP" },
			CodeLandingInvalid},
		"no expected value": {func(s *Step) { s.Landing.Value = "  " }, CodeLandingInvalid},
		"no landing at all":  {func(s *Step) { s.Landing = nil }, CodeLandingMissing},
		"unknown managed change": {func(s *Step) {
			s.Landing, s.ManagedChangeID = nil, ptr(int64(999))
		}, CodeLandingInvalid},
		"sql for pg_sage": {func(s *Step) { s.ProposedSQL = "ALTER SYSTEM SET work_mem = 1" },
			CodeSQLNotApplicable},
		"rollback sql": {func(s *Step) { s.RollbackSQL = "ALTER SYSTEM RESET work_mem" },
			CodeRollbackNotApplicable},
		"command too long": {func(s *Step) { s.OperatorCommand = strings.Repeat("x", 10001) },
			CodeTextTooLong},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := operatorPlan()
			c.edit(&p.Steps[0])
			ps := validate(t, p, newFakeResolver())
			if !hasCode(ps, c.code, 1) {
				t.Fatalf("problems %v, want %s@1", codes(ps), c.code)
			}
		})
	}
}

// The refusal names the code the plan entry asks for, so a caller can
// tell an unrevertable operator step from any other outcome problem.
func TestValidate_OperatorNotRevertibleDetail(t *testing.T) {
	p := operatorPlan()
	p.Steps[0].OnRegressed = ActionRevertStep
	for _, pr := range validate(t, p, newFakeResolver()) {
		if pr.Code == CodeOperatorNotRevertible {
			if !strings.Contains(pr.Detail, "on_regressed") ||
				!strings.Contains(pr.Detail, "halt_ask") {
				t.Fatalf("detail %q, want the field and the action to use instead", pr.Detail)
			}
			return
		}
	}
	t.Fatal("no operator_not_revertible problem")
}

// Nil/empty: no command text is refused; no managed proposal is fine
// (free-text command); an action step needs none of the operator fields
// and is refused if it carries them.
func TestValidate_OperatorStepEmptyFields(t *testing.T) {
	p := operatorPlan()
	p.Steps[0].OperatorCommand = "   "
	if ps := validate(t, p, newFakeResolver()); !hasCode(ps, CodeCommandMissing, 1) {
		t.Fatalf("blank command: %v, want %s@1", codes(ps), CodeCommandMissing)
	}
	p = operatorPlan()
	p.Steps[0].ManagedChangeID, p.Steps[0].OperatorRevert = nil, ""
	if ps := validate(t, p, newFakeResolver()); len(ps) != 0 {
		t.Fatalf("free-text command, no revert text: %v, want clean", codes(ps))
	}
	a := threeStepPlan()
	a.Steps[0].OperatorCommand = "run it yourself"
	a.Steps[1].Landing = &Landing{Setting: "work_mem", Value: "1MB"}
	ps := validate(t, a, newFakeResolver())
	if !hasCode(ps, CodeOperatorFieldsNotApplicable, 1) ||
		!hasCode(ps, CodeOperatorFieldsNotApplicable, 2) {
		t.Fatalf("action steps with operator fields: %v, want %s@1 and @2", codes(ps),
			CodeOperatorFieldsNotApplicable)
	}
}

// Error propagation: a setting lookup that fails is an error, never a
// problem and never a pass.
func TestValidate_OperatorLandingLookupFails(t *testing.T) {
	r := newFakeResolver()
	r.settingErr = errors.New("connection refused")
	ps, err := Validate(context.Background(), operatorPlan(), r)
	if err == nil || !strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "step 1") {
		t.Fatalf("err %v, want the lookup failure naming step 1", err)
	}
	if hasCode(ps, CodeLandingInvalid, 1) {
		t.Fatalf("a lookup failure was reported as an invalid landing: %v", codes(ps))
	}
}

// Defaults: an operator step's regression halts and asks (pg_sage cannot
// revert it); an action step's still reverts.
func TestWithDefaults_OperatorRegressionAsks(t *testing.T) {
	op := operatorStep(1).WithDefaults()
	if op.OnRegressed != ActionHaltAsk || op.ActionFor(OutcomeRegressed) != ActionHaltAsk {
		t.Fatalf("operator on_regressed %q, want halt_ask", op.OnRegressed)
	}
	if op.OnExpired != ActionHaltAsk || op.OnImproved != ActionKeep {
		t.Fatalf("operator defaults %+v, want expired halt_ask and improved keep", op)
	}
	if a := indexStep(1, "i", "c", 11, 101).WithDefaults(); a.OnRegressed != ActionRevertStep {
		t.Fatalf("action on_regressed %q, want revert_step", a.OnRegressed)
	}
	if ps := validate(t, operatorPlan(), newFakeResolver()); len(ps) != 0 {
		t.Fatalf("operator step with defaults: %v, want clean", codes(ps))
	}
}

// The approval binds what the operator is asked to run and how landing is
// read; an action step's hash does not move.
func TestPlanHash_BindsTheOperatorFields(t *testing.T) {
	base := PlanHash(operatorPlan())
	edits := map[string]func(*Step){
		"command":         func(s *Step) { s.OperatorCommand += " --apply-method immediate" },
		"revert":          func(s *Step) { s.OperatorRevert = "" },
		"landing setting": func(s *Step) { s.Landing.Setting = "maintenance_work_mem" },
		"landing value":   func(s *Step) { s.Landing.Value = "128MB" },
		"landing cleared": func(s *Step) { s.Landing = nil },
		"managed change":  func(s *Step) { s.ManagedChangeID = ptr(int64(5)) },
	}
	for name, edit := range edits {
		p := operatorPlan()
		edit(&p.Steps[0])
		if PlanHash(p) == base {
			t.Errorf("%s: hash unchanged", name)
		}
	}
	runtime := operatorPlan()
	runtime.Steps[0].Runtime.LandedAt = ptr(time.Now())
	if PlanHash(runtime) != base {
		t.Error("landed_at moved the hash; it is runtime")
	}
}

// The landing compares the expected value with pg_settings' raw setting
// and with SHOW's display, so "64MB" lands on a work_mem of 65536 kB.
func TestSettingMatches(t *testing.T) {
	cases := []struct {
		want, setting, display string
		ok                     bool
	}{
		{"64MB", "65536", "64MB", true},
		{"65536", "65536", "64MB", true},
		{"64mb", "65536", "64MB", true},
		{"'64MB'", "65536", "64MB", true},
		{" 64MB ", "65536", "64MB", true},
		{"128MB", "65536", "64MB", false},
		{"", "", "", false},
		{"on", "on", "on", true},
		{"off", "on", "on", false},
	}
	for _, c := range cases {
		if got := settingMatches(c.want, c.setting, c.display); got != c.ok {
			t.Errorf("settingMatches(%q, %q, %q) = %v, want %v", c.want, c.setting,
				c.display, got, c.ok)
		}
	}
}

// Boundary: the verdict wait counts from landing; a verdict one second
// before the deadline is used, one decided a second after is expired.
func TestJudgeOutcome_OperatorCountsFromLanding(t *testing.T) {
	landed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := operatorStep(1)
	s.Runtime.LandedAt = &landed
	deadline := landed.Add(60 * time.Minute)
	in := deadline.Add(-time.Second)
	o, byVerifier, done := judgeOutcome(s, ActionVerdict{Verdict: "improved",
		DecidedAt: &in}, deadline)
	if o != OutcomeImproved || byVerifier || !done {
		t.Fatalf("verdict before the deadline: %s %v %v, want improved", o, byVerifier, done)
	}
	if _, _, done := judgeOutcome(s, ActionVerdict{}, deadline); done {
		t.Fatal("no verdict at the deadline: done, want still waiting")
	}
	after := deadline.Add(time.Second)
	if o, _, done := judgeOutcome(s, ActionVerdict{Verdict: "improved", DecidedAt: &after},
		after); o != OutcomeExpired || !done {
		t.Fatalf("verdict after the deadline: %s %v, want expired", o, done)
	}
}

// A restart-bound operator step counts against max_restart_bound_steps,
// and one whose setting is unknown (a managed change with no landing
// setting) counts too (fail closed).
func TestCheckPlanCaps_OperatorRestartBound(t *testing.T) {
	ctxs := SettingContexts{"shared_buffers": "postmaster", "max_connections": "postmaster",
		"work_mem": "user"}
	restart := func(ordinal int, name string) Step {
		s := operatorStep(ordinal)
		s.Setting, s.Landing = name, &Landing{Setting: name, Value: "1"}
		return s
	}
	p := operatorPlan()
	p.Steps = []Step{restart(1, "shared_buffers"), operatorStep(2)}
	if err := CheckPlanCaps(p, policy.PlanCaps{}, ctxs); err != nil {
		t.Fatalf("one restart-bound operator step: %v, want it allowed", err)
	}
	p.Steps = append(p.Steps, restart(3, "max_connections"))
	var ce *CapError
	if err := CheckPlanCaps(p, policy.PlanCaps{}, ctxs); !errors.As(err, &ce) ||
		ce.Cap != CapRestartBoundSteps || ce.Got != 2 {
		t.Fatalf("two restart-bound operator steps: %v, want the restart cap with 2", err)
	}
	managed := operatorStep(3)
	managed.Setting, managed.Landing, managed.ManagedChangeID = "", nil, ptr(int64(4))
	managed.Metric, managed.TargetQueryIDs = MetricTimePerCall, []int64{101}
	p.Steps = []Step{restart(1, "shared_buffers"), managed}
	if err := CheckPlanCaps(p, policy.PlanCaps{}, ctxs); !errors.As(err, &ce) ||
		ce.Got != 2 {
		t.Fatalf("restart-bound plus an unnamed managed change: %v, want 2 counted", err)
	}
}

// Interference: an operator step changes a setting, so it never runs with
// another plan's step in flight, and another plan's step waits on it.
func TestConflict_OperatorStepRunsAlone(t *testing.T) {
	op := operatorStep(1)
	others := []otherStep{{planID: 9, ordinal: 1, targets: []int64{555},
		sql: "CREATE INDEX CONCURRENTLY x ON public.t (a)"}}
	if _, ok := conflict(op, others); !ok {
		t.Fatal("operator step beside an unrelated index step: no conflict, want one")
	}
	if _, ok := conflict(op, nil); ok {
		t.Fatal("operator step alone: conflict, want none")
	}
	idx := indexStep(1, "i", "c", 11, 101)
	if _, ok := conflict(idx, []otherStep{{planID: 9, ordinal: 2, operator: true}}); !ok {
		t.Fatal("index step beside another plan's operator step: no conflict, want one")
	}
}

// State transitions: awaiting_operator goes to judging (landed) or
// skipped (expired); nothing else.
func TestLegalTransitions_Operator(t *testing.T) {
	for _, to := range []StepState{StateJudging, StateSkipped} {
		if !legal(StateAwaitingOperator, to) {
			t.Errorf("awaiting_operator -> %s illegal, want legal", to)
		}
	}
	for _, to := range []StepState{StateKept, StateRunning, StateReverting, StateExecuted,
		StateQueued} {
		if legal(StateAwaitingOperator, to) {
			t.Errorf("awaiting_operator -> %s legal, want illegal", to)
		}
	}
}

// The operator judge: a readback step is decided by its landing; a
// measured step waits one window after landing, then is judged by an
// interrupted time series (no control group) and the verdict says so;
// without a verifier it is unverifiable, never improved.
func TestOperatorJudge(t *testing.T) {
	ctx := context.Background()
	landed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := operatorStep(1)
	s.Runtime.LandedAt = &landed
	v, err := NewOperatorJudge(nil, 0).JudgeOperator(ctx, s, landed.Add(time.Minute))
	if err != nil || v.Verdict != "improved" || v.DecidedAt == nil ||
		!v.DecidedAt.Equal(landed) || v.Detail["judged_by"] != "readback" {
		t.Fatalf("readback judge: %+v %v, want improved at the landing", v, err)
	}
	m := s
	m.Metric, m.TargetQueryIDs, m.Setting = MetricTimePerCall, []int64{101}, ""
	v, err = NewOperatorJudge(nil, 0).JudgeOperator(ctx, m, landed.Add(time.Hour))
	if err != nil || v.Verdict != "unverifiable" || v.Detail["reason"] == nil {
		t.Fatalf("no verifier: %+v %v, want unverifiable with a reason", v, err)
	}
	opts := &verify.V2Options{Counters: emptyCounters{}, Mechanisms: passMechanism{},
		Method: func(context.Context) string { return verify.MethodV2 }}
	j := NewOperatorJudge(opts, 30*time.Minute)
	v, err = j.JudgeOperator(ctx, m, landed.Add(29*time.Minute))
	if err != nil || v.Verdict != "" {
		t.Fatalf("before the window: %+v %v, want pending", v, err)
	}
	v, err = j.JudgeOperator(ctx, m, landed.Add(30*time.Minute))
	if err != nil || v.Verdict == "" || v.Verdict == "pending" || v.DecidedAt == nil {
		t.Fatalf("at the window: %+v %v, want a decided verdict", v, err)
	}
	if v.Detail["judged_by"] != "interrupted_time_series" ||
		v.Detail["attribution"] != verify.AttributionGlobal ||
		v.Detail["note"] != verify.ReasonGlobal {
		t.Fatalf("detail %v, want the interrupted time series named", v.Detail)
	}
	if v.Verdict == "improved" {
		t.Fatalf("no counters judged improved: %+v", v)
	}
	failing := &verify.V2Options{Counters: failCounters{}, Mechanisms: passMechanism{},
		Method: opts.Method}
	if _, err := NewOperatorJudge(failing, time.Minute).JudgeOperator(ctx, m,
		landed.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "counters down") {
		t.Fatalf("failing counters: %v, want the error", err)
	}
}

type emptyCounters struct{}

func (emptyCounters) Window(context.Context, []int64, time.Time, time.Time,
	time.Duration) (verify.CounterWindow, error) {
	return verify.CounterWindow{}, nil
}

func (emptyCounters) Seasonal(context.Context, []int64, time.Time, time.Time,
	int) (verify.Series, error) {
	return verify.Series{}, nil
}

type failCounters struct{ emptyCounters }

func (failCounters) Window(context.Context, []int64, time.Time, time.Time,
	time.Duration) (verify.CounterWindow, error) {
	return verify.CounterWindow{}, errors.New("counters down")
}

type passMechanism struct{}

func (passMechanism) CheckMechanism(_ context.Context, in verify.MechanismInput,
	_ time.Time) (verify.Mechanism, error) {
	return verify.MechanismCheck(in)
}
