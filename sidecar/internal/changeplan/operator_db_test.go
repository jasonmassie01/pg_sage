package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// plan-02d on a real database: the engine asks the operator once, reads
// the landing from pg_settings or the managed proposal, judges the landed
// change, never reverts it and expires a step that does not land in time.
// The "operator" sets the setting on the fixture's own database (ALTER
// DATABASE ... SET, then fresh connections), so the cluster-wide settings
// other packages' tests read are never touched.

// fakeOperatorJudge decides every landed operator step's verdict.
type fakeOperatorJudge struct {
	mu      sync.Mutex
	verdict string
	calls   int
}

func (j *fakeOperatorJudge) JudgeOperator(_ context.Context, s Step,
	now time.Time) (ActionVerdict, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	if j.verdict == "" {
		return ActionVerdict{}, nil
	}
	return ActionVerdict{Verdict: j.verdict, DecidedAt: &now,
		Detail: map[string]any{"judged_by": "fake"}}, nil
}

// failingLandings fails every landing read.
type failingLandings struct{}

func (failingLandings) CheckLanding(context.Context, Step, time.Time) (LandingCheck, error) {
	return LandingCheck{}, errors.New("pg_settings read failed: connection reset")
}

func (failingLandings) RevertCommand(context.Context, Step) (string, error) {
	return "", nil
}

// operatorEngine replaces the fixture's engine with one that judges
// operator steps with judge and reads landings with landings (nil: the
// database).
func (f *engineFixture) operatorEngine(judge OperatorJudge, landings LandingReader) {
	f.t.Helper()
	e, err := NewEngine(EngineConfig{Pool: f.pool, Executor: f.exec, Waits: f.waits,
		Verdicts: f.verdicts, Notify: f.notes, Leader: f.leader.Load, Now: f.clock.Now,
		Owner: "engine-a", Database: "db1", Caps: f.readCaps, Sizes: fixtureSizes{f},
		Stopped:  func(context.Context) bool { return f.stopped.Load() },
		Operator: judge, Landings: landings})
	if err != nil {
		f.t.Fatalf("NewEngine: %v", err)
	}
	f.engine = e
}

// operate is the operator applying work_mem on the fixture's database.
func (f *engineFixture) operate(value string) {
	f.t.Helper()
	var db string
	if err := f.pool.QueryRow(f.ctx, `SELECT current_database()`).Scan(&db); err != nil {
		f.t.Fatalf("current database: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, fmt.Sprintf(`ALTER DATABASE %q SET work_mem = %s`, db,
		quoteLiteral(value))); err != nil {
		f.t.Fatalf("operator: %v", err)
	}
	f.pool.Reset() // new sessions read the database's setting
}

func quoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

func (f *engineFixture) step1(id int64) StepRuntime {
	f.t.Helper()
	return f.get(id).Steps[0].Runtime
}

// Happy path: the step asks once, waits until work_mem reads 77MB, is
// judged and kept; the plan completes and pg_sage ran nothing.
func TestOperatorStep_LandsIsJudgedAndKept(t *testing.T) {
	f := newEngineFixture(t, "op_land")
	f.operatorEngine(nil, nil)
	p := operatorPlan()
	p.Steps[0].Landing.Value = "77MB"
	p = f.approved(p)
	f.tick()
	if rt := f.step1(p.ID); rt.State != StateAwaitingOperator || rt.WaitingSince == nil {
		t.Fatalf("after one tick: %+v, want awaiting_operator since now", rt)
	}
	for range 3 {
		f.clock.Add(time.Minute)
		f.tick()
	}
	rt := f.step1(p.ID)
	if rt.State != StateAwaitingOperator || rt.LandedAt != nil ||
		!strings.Contains(rt.StateReason, "work_mem") {
		t.Fatalf("before the operator ran it: %+v, want still awaiting with the reading", rt)
	}
	asks := 0
	for _, n := range f.notes.sent() {
		if strings.Contains(n.Body, p.Steps[0].OperatorCommand) &&
			strings.Contains(n.Body, "work_mem") {
			asks++
		}
	}
	if asks != 1 || len(f.notes.sent()) != 1 {
		t.Fatalf("notifications %+v, want one naming the command", f.notes.sent())
	}
	f.operate("77MB")
	f.clock.Add(time.Minute)
	f.tick()
	rt = f.step1(p.ID)
	if rt.State != StateJudging || rt.LandedAt == nil || !rt.LandedAt.Equal(f.clock.Now()) {
		t.Fatalf("after landing: %+v, want judging landed now", rt)
	}
	got := f.runUntil(p.ID, "improved", 5)
	if got.Status != StatusCompleted || got.Steps[0].Runtime.State != StateKept ||
		got.Steps[0].Runtime.Verdict != OutcomeImproved {
		t.Fatalf("plan %s step %+v, want completed and kept improved", got.Status,
			got.Steps[0].Runtime)
	}
	if f.manual.callCount() != 0 || len(f.queueItems(p.ID)) != 0 ||
		f.exec.rollbackCount() != 0 {
		t.Fatal("pg_sage queued, ran or rolled back something for an operator step")
	}
	assertOperatorEvents(t, f, p.ID)
}

// assertOperatorEvents: one event per move, the ask naming the command.
func assertOperatorEvents(t *testing.T, f *engineFixture, id int64) {
	t.Helper()
	want := map[string]int{"pending>awaiting_operator": 1, "awaiting_operator>judging": 1,
		"judging>kept": 1}
	got := map[string]int{}
	for _, e := range f.events(id) {
		from, _ := e.Detail["from"].(string)
		to, _ := e.Detail["to"].(string)
		if from == "" {
			continue
		}
		got[from+">"+to]++
		if to == string(StateAwaitingOperator) && e.Detail["operator_command"] == nil {
			t.Errorf("the ask's event %v does not name the command", e.Detail)
		}
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("step events %v, want %v", got, want)
		}
	}
}

// State transitions: a regression halts the plan with the operator's
// revert command in the event and the notification; pg_sage reverts
// nothing.
func TestOperatorStep_RegressionHaltsWithTheRevertCommand(t *testing.T) {
	f := newEngineFixture(t, "op_regress")
	judge := &fakeOperatorJudge{verdict: "regressed"}
	f.operatorEngine(judge, nil)
	p := operatorPlan()
	p.Steps[0].Landing.Value = "78MB"
	p.Steps[0].Metric, p.Steps[0].TargetQueryIDs = MetricTimePerCall, []int64{101}
	p.Steps[0].ExpectedDirection, p.Steps[0].Setting = DirectionDown, ""
	p = f.approved(p)
	f.tick()
	f.operate("78MB")
	for range 4 {
		f.clock.Add(time.Minute)
		f.tick()
	}
	got := f.get(p.ID)
	rt := got.Steps[0].Runtime
	if got.Status != StatusHalted || rt.State != StateHalted || rt.Verdict != OutcomeRegressed {
		t.Fatalf("plan %s step %+v, want halted on regressed", got.Status, rt)
	}
	revert := p.Steps[0].OperatorRevert
	if !strings.Contains(rt.StateReason, revert) {
		t.Fatalf("reason %q, want the revert command", rt.StateReason)
	}
	found := false
	for _, e := range f.events(p.ID) {
		if e.Detail["to"] == string(StateHalted) && e.Detail["operator_revert"] == revert {
			found = true
		}
	}
	notified := false
	for _, n := range f.notes.sent() {
		notified = notified || (strings.Contains(n.Subject, "halted") &&
			strings.Contains(n.Body, revert))
	}
	if !found || !notified || f.exec.rollbackCount() != 0 || judge.calls == 0 {
		t.Fatalf("event %v notified %v rollbacks %d judged %d: want the revert command in "+
			"both and no rollback", found, notified, f.exec.rollbackCount(), judge.calls)
	}
}

// Boundary: landing at the deadline minute is used; one minute after is
// expired, and the default on_expired (halt_ask) halts the plan.
func TestOperatorStep_DeadlineBoundary(t *testing.T) {
	for _, c := range []struct {
		name  string
		after time.Duration
		want  StepState
	}{{"at the deadline", 60 * time.Minute, StateJudging},
		{"a minute late", 61 * time.Minute, StateSkipped}} {
		t.Run(c.name, func(t *testing.T) {
			f := newEngineFixture(t, "op_deadline")
			f.operatorEngine(nil, nil)
			p := operatorPlan()
			p.Steps[0].Landing.Value = "79MB"
			p = f.approved(p)
			f.tick()
			f.operate("79MB")
			f.clock.Add(c.after)
			f.tick()
			got := f.get(p.ID)
			rt := got.Steps[0].Runtime
			if rt.State != c.want {
				t.Fatalf("step %+v, want %s", rt, c.want)
			}
			if c.want != StateSkipped {
				return
			}
			if rt.Verdict != OutcomeExpired || rt.LandedAt != nil ||
				!strings.HasPrefix(rt.StateReason, ReasonOperatorNotLanded) {
				t.Fatalf("expired step %+v, want verdict expired and %s", rt,
					ReasonOperatorNotLanded)
			}
			f.tick()
			if got = f.get(p.ID); got.Status != StatusHalted {
				t.Fatalf("plan %s, want halted on an expired operator step", got.Status)
			}
		})
	}
}

// Error propagation: a failed landing read keeps the step waiting with the
// error, never landed.
func TestOperatorStep_FailedReadKeepsWaiting(t *testing.T) {
	f := newEngineFixture(t, "op_readfail")
	f.operatorEngine(nil, failingLandings{})
	p := f.approved(operatorPlan())
	for range 4 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	rt := f.step1(p.ID)
	if rt.State != StateAwaitingOperator || rt.LandedAt != nil ||
		!strings.Contains(rt.StateReason, "connection reset") {
		t.Fatalf("step %+v, want awaiting_operator with the read error", rt)
	}
	if got := f.get(p.ID); got.Status != StatusRunning {
		t.Fatalf("plan %s, want running", got.Status)
	}
}

// Integration: a managedparam proposal marked applied by its own store
// lands the step at the proposal's applied_at; a halt names the
// proposal's rollback command when the step gave none.
func TestOperatorStep_ManagedProposalLands(t *testing.T) {
	f := newEngineFixture(t, "op_managed")
	judge := &fakeOperatorJudge{verdict: "regressed"}
	f.operatorEngine(judge, nil)
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.managed_change_proposals
		(fingerprint, provider, mechanism, target, parameter, value, reboot_required, proposal)
		VALUES ('fp-op', 'rds', 'parameter_group', 'pg-1', 'work_mem', '65536', false,
		        '{"rollback": {"cli": "aws rds reset-db-parameter-group --name work_mem"}}')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert the proposal: %v", err)
	}
	p := operatorPlan()
	s := &p.Steps[0]
	s.Landing, s.ManagedChangeID, s.OperatorRevert = nil, &id, ""
	s.Metric, s.TargetQueryIDs, s.ExpectedDirection, s.Setting = MetricTimePerCall,
		[]int64{101}, DirectionDown, ""
	r := newFakeResolver()
	r.managed[id] = true
	created, err := f.store.Create(f.ctx, p, r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.store.Approve(f.ctx, created.ID, created.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	f.tick()
	f.clock.Add(time.Minute)
	f.tick()
	if rt := f.step1(created.ID); rt.State != StateAwaitingOperator {
		t.Fatalf("before applied: %+v, want awaiting_operator", rt)
	}
	applied := f.clock.Now().Add(-30 * time.Second).Truncate(time.Microsecond)
	if err := managedparam.NewStore(f.pool).MarkApplied(f.ctx, id, applied); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	f.tick()
	rt := f.step1(created.ID)
	if rt.State != StateJudging || rt.LandedAt == nil || !rt.LandedAt.Equal(applied) {
		t.Fatalf("after applied: %+v, want judging landed at %s", rt, applied)
	}
	f.clock.Add(time.Minute)
	f.tick()
	if rt = f.step1(created.ID); rt.State != StateHalted ||
		!strings.Contains(rt.StateReason, "aws rds reset-db-parameter-group --name work_mem") {
		t.Fatalf("regressed: %+v, want halted naming the proposal's rollback", rt)
	}
}

// Invalid input at create: two restart-bound operator steps are refused
// by the plan cap (their settings' contexts are read from pg_settings).
func TestOperatorStep_RestartBoundCapAtCreate(t *testing.T) {
	f := defaultCaps(t, "op_restart_cap")
	p := operatorPlan()
	second := operatorStep(2)
	for i, name := range []string{"shared_buffers", "max_connections"} {
		st := &p.Steps[0]
		if i == 1 {
			p.Steps = append(p.Steps, second)
			st = &p.Steps[1]
		}
		st.Setting, st.Landing = name, &Landing{Setting: name, Value: "100"}
	}
	_, err := f.store.Create(f.ctx, p, newFakeResolver())
	var ce *CapError
	if !errors.As(err, &ce) || ce.Cap != CapRestartBoundSteps || ce.Got != 2 {
		t.Fatalf("create: %v, want the restart-bound cap with 2", err)
	}
	p.Steps = p.Steps[:1]
	if _, err := f.store.Create(f.ctx, p, newFakeResolver()); err != nil {
		t.Fatalf("one restart-bound operator step: %v, want it created", err)
	}
}

// A setting change runs alone: an operator step is not asked for while
// another plan's step is in flight on the database, and is asked for once
// that step has its verdict.
func TestOperatorStep_WaitsForOtherPlansInFlight(t *testing.T) {
	f := newEngineFixture(t, "op_alone")
	f.operatorEngine(&fakeOperatorJudge{}, nil)
	idx := f.approved(chain(1, 101))
	f.tick() // the index step starts and runs
	op := f.approved(operatorPlan())
	f.tick()
	f.tick()
	if rt := f.step1(op.ID); rt.State != StatePending {
		t.Fatalf("operator step beside an in-flight step: %+v, want pending", rt)
	}
	f.runUntil(idx.ID, "improved", 10)
	for range 3 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	if rt := f.step1(op.ID); rt.State != StateAwaitingOperator {
		t.Fatalf("after the other plan finished: %+v, want awaiting_operator", rt)
	}
}

// Another plan's step waits while an operator step awaits its operator
// (the operator may apply it at any moment), and starts once it is kept.
func TestOperatorStep_OtherPlansWaitOnAnAwaitingOperatorStep(t *testing.T) {
	f := newEngineFixture(t, "op_blocks")
	f.operatorEngine(nil, nil)
	op := f.approved(operatorPlan())
	f.tick()
	if rt := f.step1(op.ID); rt.State != StateAwaitingOperator {
		t.Fatalf("operator step: %+v, want awaiting_operator", rt)
	}
	idx := f.approved(chain(1, 101))
	for range 3 {
		f.clock.Add(time.Minute)
		f.tick()
	}
	if rt := f.step1(idx.ID); rt.State != StatePending || rt.QueueID != nil {
		t.Fatalf("index step beside an awaiting operator step: %+v, want pending", rt)
	}
	f.operate("64MB")
	for range 3 {
		f.clock.Add(time.Minute)
		f.tick()
	}
	if rt := f.step1(op.ID); rt.State != StateKept {
		t.Fatalf("operator step after landing: %+v, want kept", rt)
	}
	if rt := f.step1(idx.ID); rt.State == StatePending {
		t.Fatalf("index step after the operator step was kept: %+v, want started", rt)
	}
}
