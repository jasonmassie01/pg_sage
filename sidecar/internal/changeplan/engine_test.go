package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// plan-02a: the engine runs an approved plan one step at a time through
// the executor's approval path, waiting for each step's verdict before the
// next starts. These tests use a fake executor and a fake verifier over a
// real database (engine_fake_test.go).

func TestEngine_HappyPathRunsTheStepsInOrder(t *testing.T) {
	f := newEngineFixture(t, "engine_happy")
	p := f.approved(chain(3, 101))
	got := f.runUntil(p.ID, "improved", 40)
	if got.Status != StatusCompleted {
		t.Fatalf("plan %s after 40 ticks, want completed: %+v", got.Status, got.Steps)
	}
	items := f.queueItems(p.ID)
	if len(items) != 3 || f.manual.callCount() != 3 {
		t.Fatalf("%d queue items, %d runs, want 3 of each", len(items), f.manual.callCount())
	}
	for i, s := range got.Steps {
		item := items[i]
		wantKey := fmt.Sprintf("plan_step:%d:%d", p.ID, s.ID)
		if item.IdentityKey != wantKey || item.ProposedSQL != s.ProposedSQL ||
			item.DecidedBy == nil || *item.DecidedBy != 7 || item.Status != "executed" ||
			item.ActionLogID == nil || item.ProposedBy != fmt.Sprintf("plan:%d", p.ID) {
			t.Fatalf("step %d queue item %+v, want %s approved by 7 with the step's SQL",
				s.Ordinal, item, wantKey)
		}
		if s.Runtime.State != StateKept || s.Runtime.Verdict != OutcomeImproved ||
			s.Runtime.QueueID == nil || int(*s.Runtime.QueueID) != item.ID ||
			s.Runtime.ActionLogID == nil || *s.Runtime.ActionLogID != *item.ActionLogID ||
			s.Runtime.Attempt != 1 {
			t.Fatalf("step %d runtime %+v, want kept/improved linked to item %d", s.Ordinal,
				s.Runtime, item.ID)
		}
		call := f.manual.calls[i]
		if call.sql != s.ProposedSQL || call.approver != 7 || call.findingID != int(*s.FindingID) {
			t.Fatalf("run %d = %+v, want step %d's SQL for approver 7", i, call, s.Ordinal)
		}
		if i > 0 && call.at.Before(*got.Steps[i-1].Runtime.VerdictAt) {
			t.Fatalf("step %d ran at %v before step %d's verdict at %v", s.Ordinal, call.at,
				i, *got.Steps[i-1].Runtime.VerdictAt)
		}
	}
	ev := f.events(p.ID)
	if countKind(ev, EventCompleted) != 1 || countKind(ev, EventVerdict) < 3 ||
		countKind(ev, EventStepExecuted) < 3 || countKind(ev, EventHalted) != 0 {
		t.Fatalf("events %v, want one completed and a verdict and execution per step",
			eventKinds(ev))
	}
}

// Invalid input: the gate refuses step 1's SQL; it fails with the gate's
// reason, the plan halts with one event and one notification, and nothing
// later starts.
func TestEngine_GateRefusalFailsTheStepAndHalts(t *testing.T) {
	f := newEngineFixture(t, "engine_refused")
	p := f.approved(chain(2, 101))
	f.manual.refuse[p.Steps[0].ProposedSQL] = "trust level does not allow DDL"
	got := f.runUntil(p.ID, "improved", 10)
	for range 5 {
		f.tick()
	}
	got = f.get(p.ID)
	s1, s2 := got.Steps[0].Runtime, got.Steps[1].Runtime
	if got.Status != StatusHalted || s1.State != StateFailed ||
		!strings.Contains(s1.StateReason, "trust level does not allow DDL") ||
		s2.State != StatePending || f.manual.callCount() != 0 {
		t.Fatalf("plan %s, step 1 %+v, step 2 %+v, runs %d: want halted, failed with the "+
			"gate's reason, step 2 pending", got.Status, s1, s2, f.manual.callCount())
	}
	if n := countKind(f.events(p.ID), EventHalted); n != 1 {
		t.Fatalf("%d halted events, want 1", n)
	}
	sent := f.notes.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Subject, fmt.Sprint(p.ID)) ||
		!strings.Contains(sent[0].Body, "step 1") ||
		!strings.Contains(sent[0].Body, "trust level does not allow DDL") {
		t.Fatalf("notifications %+v, want one naming the plan, the step and why", sent)
	}
}

// Nil/empty: a plan with no approval never starts.
func TestEngine_UnapprovedPlanNeverStarts(t *testing.T) {
	f := newEngineFixture(t, "engine_unapproved")
	p := mustCreate(t, f.store, chain(2, 101))
	for range 5 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	got := f.get(p.ID)
	if got.Status != StatusProposed || got.Steps[0].Runtime.State != StatePending ||
		len(f.queueItems(p.ID)) != 0 || f.manual.callCount() != 0 {
		t.Fatalf("unapproved plan %s steps %+v: want proposed and untouched", got.Status,
			got.Steps)
	}
}

// Nil/empty: a check-only plan completes without a queue item.
func TestEngine_CheckOnlyPlanCompletesWithoutAQueueItem(t *testing.T) {
	f := newEngineFixture(t, "engine_check")
	in := chain(1, 101)
	in.Steps[0] = checkStep(1, "")
	p := f.approved(in)
	got := f.runUntil(p.ID, "improved", 5)
	if got.Status != StatusCompleted || got.Steps[0].Runtime.State != StateKept ||
		len(f.queueItems(p.ID)) != 0 || f.manual.callCount() != 0 {
		t.Fatalf("check plan %s step %+v, want completed with no queue item", got.Status,
			got.Steps[0].Runtime)
	}
}

// Error propagation: a queue insert failure leaves the step starting with
// the error recorded, never queued, and nothing else starts.
func TestEngine_QueueFailureLeavesTheStepStarting(t *testing.T) {
	f := newEngineFixture(t, "engine_queue_fail")
	mustExecPlan(t, f, `CREATE FUNCTION public.refuse_plan_items() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'queue is read-only today'; END $$;
		CREATE TRIGGER refuse_plan_items BEFORE INSERT ON sage.action_queue
		FOR EACH ROW EXECUTE FUNCTION public.refuse_plan_items()`)
	p := f.approved(chain(2, 101))
	for range 4 {
		f.tick()
	}
	got := f.get(p.ID)
	s1 := got.Steps[0].Runtime
	if s1.State != StateStarting || s1.QueueID != nil ||
		!strings.Contains(s1.StateReason, "queue is read-only today") ||
		got.Steps[1].Runtime.State != StatePending || f.manual.callCount() != 0 {
		t.Fatalf("step 1 %+v, step 2 %s: want starting with the queue error, step 2 pending",
			s1, got.Steps[1].Runtime.State)
	}
}

// Error propagation: RunApprovedAction's error fails the step with its text.
func TestEngine_RunErrorFailsTheStepWithItsText(t *testing.T) {
	f := newEngineFixture(t, "engine_run_err")
	f.exec.runErr = errBoom
	p := f.approved(chain(1, 101))
	f.tick()
	got := f.get(p.ID)
	s1 := got.Steps[0].Runtime
	if s1.State != StateFailed || !strings.Contains(s1.StateReason, errBoom.Error()) ||
		s1.QueueID == nil {
		t.Fatalf("step 1 %+v, want failed with %q and its queue item", s1, errBoom)
	}
	f.tick()
	if got := f.get(p.ID); got.Status != StatusHalted {
		t.Fatalf("plan %s after a failed step, want halted", got.Status)
	}
}

// State transitions: an illegal transition is an error and changes
// nothing; a hash mismatch voids the plan before its step starts.
func TestEngine_IllegalTransitionChangesNothing(t *testing.T) {
	f := newEngineFixture(t, "engine_illegal")
	p := f.approved(chain(1, 101))
	got := f.runUntil(p.ID, "improved", 10)
	if got.Steps[0].Runtime.State != StateKept {
		t.Fatalf("step 1 %s, want kept", got.Steps[0].Runtime.State)
	}
	before := len(f.events(p.ID))
	err := f.engine.transition(f.ctx, p.ID, got.Steps[0].ID, StateKept, StateRunning,
		stepWrite{reason: "again"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("kept -> running = %v, want ErrIllegalTransition", err)
	}
	if after := f.get(p.ID).Steps[0].Runtime; after.State != StateKept ||
		after.StateReason == "again" || len(f.events(p.ID)) != before {
		t.Fatalf("an illegal transition changed the step (%+v) or wrote an event", after)
	}
	// A legal transition from a state the step is not in is refused too.
	err = f.engine.transition(f.ctx, p.ID, got.Steps[0].ID, StatePending, StateStarting,
		stepWrite{})
	if !errors.Is(err, ErrStepMoved) {
		t.Fatalf("pending -> starting on a kept step = %v, want ErrStepMoved", err)
	}
}

func TestEngine_HashMismatchVoidsThePlanBeforeItsStep(t *testing.T) {
	f := newEngineFixture(t, "engine_void")
	p := f.approved(chain(2, 101))
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.plan_step SET proposed_sql =
		'CREATE INDEX CONCURRENTLY idx_evil ON public.orders (c1)' WHERE plan_id = %d
		AND ordinal = 1`, p.ID))
	f.tick()
	got := f.get(p.ID)
	if got.Status != StatusVoided || got.Steps[0].Runtime.State != StatePending ||
		len(f.queueItems(p.ID)) != 0 {
		t.Fatalf("edited plan %s step %s, want voided before step 1 starts", got.Status,
			got.Steps[0].Runtime.State)
	}
	ev := f.events(p.ID)
	last := ev[len(ev)-1]
	if last.Kind != EventVoided || !strings.Contains(fmt.Sprint(last.Detail), "hash_mismatch") {
		t.Fatalf("last event %+v, want voided naming hash_mismatch", last)
	}
}

// Every legal transition writes its event: a full run records the step's
// path pending -> starting -> queued -> running -> executed -> judging ->
// kept, one event each.
func TestEngine_EveryTransitionWritesItsEvent(t *testing.T) {
	f := newEngineFixture(t, "engine_events")
	p := f.approved(chain(1, 101))
	f.runUntil(p.ID, "improved", 10)
	var path []string
	for _, e := range f.events(p.ID) {
		if e.StepID != nil {
			path = append(path, fmt.Sprintf("%v>%v", e.Detail["from"], e.Detail["to"]))
		}
	}
	want := "pending>starting,starting>queued,queued>running,running>executed," +
		"executed>judging,judging>kept"
	if strings.Join(path, ",") != want {
		t.Fatalf("step events %v, want %s", path, want)
	}
}

// Concurrency: a non-leader never transitions; two forced leaders racing
// on one plan start its step exactly once (FOR UPDATE SKIP LOCKED and the
// claim), 50 times.
func TestEngine_OnlyOneEngineTransitionsAPlan(t *testing.T) {
	f := newEngineFixture(t, "engine_two")
	follower := &atomic.Bool{}
	b := f.newEngine("engine-b", follower)
	p := f.approved(chain(1, 101))
	for range 3 {
		if err := b.Tick(f.ctx); err != nil {
			t.Fatalf("follower tick: %v", err)
		}
	}
	if got := f.get(p.ID); got.Steps[0].Runtime.State != StatePending {
		t.Fatalf("a non-leader moved step 1 to %s", got.Steps[0].Runtime.State)
	}
	follower.Store(true)
	for run := range 50 {
		q := f.approved(chain(1, 102))
		var wg sync.WaitGroup
		for _, e := range []*Engine{f.engine, b} {
			wg.Add(1)
			go func() { defer wg.Done(); _ = e.Tick(f.ctx) }()
		}
		wg.Wait()
		got := f.get(q.ID)
		started := 0
		for _, e := range f.events(q.ID) {
			if e.Detail["to"] == string(StateStarting) {
				started++
			}
		}
		if started != 1 || got.Steps[0].Runtime.Attempt != 1 {
			t.Fatalf("run %d: %d starts, attempt %d, want exactly one", run, started,
				got.Steps[0].Runtime.Attempt)
		}
		// Whichever engine claimed the step judges it; the other waits.
		both := func() {
			for _, e := range []*Engine{f.engine, b} {
				if err := e.Tick(f.ctx); err != nil {
					t.Fatalf("tick: %v", err)
				}
			}
		}
		both() // executed -> judging
		f.judgeAll(q.ID, "improved", "monitoring")
		both() // kept
		both() // completed
		if got := f.get(q.ID); got.Status != StatusCompleted {
			t.Fatalf("run %d: plan %s, want completed before the next run", run, got.Status)
		}
	}
}

// Emergency stop: nothing starts or moves, the plan shows why, and it
// resumes when the stop is lifted.
func TestEngine_EmergencyStopPausesThePlan(t *testing.T) {
	f := newEngineFixture(t, "engine_stop")
	p := f.approved(chain(2, 101))
	f.tick() // step 1 runs and is executed
	f.tick() // judging
	f.stopped.Store(true)
	f.judgeAll(p.ID, "improved", "monitoring")
	for range 3 {
		f.tick()
	}
	got := f.get(p.ID)
	if got.PauseReason != "emergency stop" || got.Steps[0].Runtime.State != StateJudging ||
		got.Steps[1].Runtime.State != StatePending || f.manual.callCount() != 1 {
		t.Fatalf("stopped plan pause %q steps %s/%s runs %d: want paused, judging, pending",
			got.PauseReason, got.Steps[0].Runtime.State, got.Steps[1].Runtime.State,
			f.manual.callCount())
	}
	f.stopped.Store(false)
	got = f.runUntil(p.ID, "improved", 20)
	if got.Status != StatusCompleted || got.PauseReason != "" {
		t.Fatalf("after the stop plan %s pause %q, want completed and unpaused", got.Status,
			got.PauseReason)
	}
}

// The plan runner refuses a plan step's queue item that the engine did not
// hand it (an Actions approval, a card, the scheduled runner): even with
// the step queued and claimed, nothing runs.
func TestPlanRunner_RefusesItemsOutsideTheEngine(t *testing.T) {
	f := newEngineFixture(t, "engine_runner")
	p := f.approved(chain(1, 101))
	f.exec.runErr = errBoom // the engine's own run fails, leaving the item queued
	f.tick()
	items := f.queueItems(p.ID)
	if len(items) != 1 {
		t.Fatalf("%d queue items, want 1", len(items))
	}
	f.exec.runErr = nil
	runner := f.engine.Runner(f.manual)
	if !runner.Owns(items[0]) {
		t.Fatal("the plan runner does not own its plan step item")
	}
	_, err := runner.RunApproved(context.Background(), items[0], 7)
	if !errors.Is(err, ErrNotEngineRun) || f.manual.callCount() != 0 {
		t.Fatalf("run outside the engine = %v, runs %d: want ErrNotEngineRun, none", err,
			f.manual.callCount())
	}
	other := items[0]
	other.IdentityKey = "sre_maintenance:x"
	if runner.Owns(other) {
		t.Fatal("the plan runner owns a non-plan item")
	}
}

// ListNeedingPerson: proposed and halted plans, and operator steps.
func TestStore_ListNeedingPerson(t *testing.T) {
	f := newEngineFixture(t, "engine_inbox")
	proposed := mustCreate(t, f.store, chain(1, 101))
	halted := f.approved(chain(1, 102))
	f.manual.refuse[halted.Steps[0].ProposedSQL] = "refused"
	for range 3 {
		f.tick()
	}
	done := f.approved(chain(1, 101))
	f.runUntil(done.ID, "improved", 10)
	got, err := f.store.ListNeedingPerson(f.ctx)
	if err != nil {
		t.Fatalf("ListNeedingPerson: %v", err)
	}
	seen := map[int64]Status{}
	for _, n := range got {
		seen[n.PlanID] = n.Status
	}
	if seen[proposed.ID] != StatusProposed || seen[halted.ID] != StatusHalted ||
		len(seen) != 2 {
		t.Fatalf("needing a person %+v, want plan %d proposed and %d halted only", got,
			proposed.ID, halted.ID)
	}
}

func mustExecPlan(t *testing.T, f *engineFixture, sql string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql); err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}
