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

// plan-02e: a sidecar killed mid-plan, and a leader change, resume each
// in-flight step from what may have happened: never a step run twice,
// never a plan stopped silently. The kill is a panic inside the fakes
// (the database keeps exactly what was committed); the restart is a new
// engine, under another owner, on the same database.

var errCrash = errors.New("crash: the sidecar was killed")

// crashTick ticks e and swallows the kill (errCrash); any other panic
// fails the test.
func (f *engineFixture) crashTick(e *Engine) {
	f.t.Helper()
	defer func() {
		if r := recover(); r != nil && r != errCrash {
			panic(r)
		}
	}()
	_ = e.Tick(f.ctx)
}

// restart is a new engine (engine-b) on the fixture's database after the
// old engine's claims lapsed; the kill hooks are cleared.
func (f *engineFixture) restart() *Engine {
	f.t.Helper()
	f.manual.mu.Lock()
	f.manual.crash = nil
	f.manual.mu.Unlock()
	f.clock.Add(DefaultClaimLease + time.Second)
	return f.newEngine("engine-b", f.leader)
}

func (f *engineFixture) tickWith(e *Engine) {
	f.t.Helper()
	if err := e.Tick(f.ctx); err != nil {
		f.t.Fatalf("tick: %v", err)
	}
}

// stepAt is step ordinal of plan id.
func (f *engineFixture) stepAt(id int64, ordinal int) Step {
	f.t.Helper()
	for _, s := range f.get(id).Steps {
		if s.Ordinal == ordinal {
			return s
		}
	}
	f.t.Fatalf("plan %d has no step %d", id, ordinal)
	return Step{}
}

// ledgerRuns counts the action_log rows that ran sql.
func (f *engineFixture) ledgerRuns(sql string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, sql).Scan(&n); err != nil {
		f.t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

// keepStep1 runs plan p's step 1 to kept.
func (f *engineFixture) keepStep1(p Plan) {
	f.t.Helper()
	for range 4 { // start and run, judge-start, then the verdict
		f.judgeAll(p.ID, "improved", "monitoring")
		f.tick()
		if f.stepAt(p.ID, 1).Runtime.State == StateKept {
			break
		}
	}
	if s := f.stepAt(p.ID, 1); s.Runtime.State != StateKept {
		f.t.Fatalf("step 1 is %s (%s), want kept", s.Runtime.State, s.Runtime.StateReason)
	}
}

// killDuringStep2 kills the engine while step 2 runs: at "approved"
// (between its approval and its run), "before" (the run started, nothing
// in the ledger) or "after" (the ledger row written, the step not yet
// told).
func (f *engineFixture) killDuringStep2(p Plan, at string) {
	f.t.Helper()
	f.keepStep1(p)
	switch at {
	case "approved":
		f.exec.beforeRun = func() { panic(errCrash) }
	default:
		f.manual.mu.Lock()
		f.manual.crash = func(stage string) {
			if stage == at {
				panic(errCrash)
			}
		}
		f.manual.mu.Unlock()
	}
	f.crashTick(f.engine)
	f.exec.beforeRun = nil
}

// Happy path: killed after step 2's change ran (its ledger row written,
// the step still running); the restart resumes at step 2's verdict wait,
// runs nothing again, and the plan completes.
func TestResume_KillAfterStep2RunsResumesAtItsVerdictWait(t *testing.T) {
	f := newEngineFixture(t, "resume_happy")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "after")
	s2 := f.stepAt(p.ID, 2)
	if s2.Runtime.State != StateRunning || f.manual.callCount() != 2 {
		t.Fatalf("after the kill step 2 is %s with %d runs, want running with 2",
			s2.Runtime.State, f.manual.callCount())
	}
	b := f.restart()
	f.tickWith(b)
	s2 = f.stepAt(p.ID, 2)
	if s2.Runtime.State != StateJudging || s2.Runtime.ActionLogID == nil ||
		s2.Runtime.Verdict != "" || s2.Runtime.ClaimedBy != "engine-b" {
		t.Fatalf("after the restart step 2 is %+v, want judging its own action, no verdict, "+
			"claimed by engine-b", s2.Runtime)
	}
	if n := f.ledgerRuns(s2.ProposedSQL); n != 1 || f.manual.callCount() != 2 {
		t.Fatalf("step 2 ran %d times (%d ledger rows), want once", f.manual.callCount()-1, n)
	}
	items := f.queueItems(p.ID)
	if len(items) != 2 || items[1].Status != "executed" ||
		items[1].ActionLogID == nil || *items[1].ActionLogID != *s2.Runtime.ActionLogID {
		t.Fatalf("queue items %+v, want step 2's one item executed with its action", items)
	}
	f.engine = b
	got := f.runUntil(p.ID, "improved", 20)
	if got.Status != StatusCompleted || f.manual.callCount() != 3 {
		t.Fatalf("plan %s after %d runs, want completed after 3", got.Status,
			f.manual.callCount())
	}
}

// State transitions: each kill point resumes the one way the resume
// table says.
func TestResume_EachKillPointResumesOneWay(t *testing.T) {
	for _, c := range []struct {
		at    string
		want  StepState
		runs  int // ExecuteManual calls over the whole test
		halts bool
	}{
		{"approved", StateExecuted, 2, false}, // queued, approved item: run once
		{"before", StateHalted, 1, true},      // running, nothing in the ledger
	} {
		t.Run(c.at, func(t *testing.T) {
			f := newEngineFixture(t, "resume_kill_"+c.at)
			p := f.approved(chain(3, 102))
			f.killDuringStep2(p, c.at)
			b := f.restart()
			f.tickWith(b)
			s2 := f.stepAt(p.ID, 2)
			if c.want == StateExecuted && s2.Runtime.State == StateJudging {
				s2.Runtime.State = StateExecuted // the same tick may judge-start it
			}
			if s2.Runtime.State != c.want || f.manual.callCount() != c.runs {
				t.Fatalf("step 2 %s (%s) after %d runs, want %s after %d",
					s2.Runtime.State, s2.Runtime.StateReason, f.manual.callCount(), c.want,
					c.runs)
			}
			f.checkHalt(p.ID, c.halts)
		})
	}
}

// checkHalt asserts plan id halted interrupted_unknown with one
// notification (halts) or did not halt at all.
func (f *engineFixture) checkHalt(id int64, halts bool) {
	f.t.Helper()
	got := f.get(id)
	ev := f.events(id)
	if !halts {
		if got.Status == StatusHalted || countKind(ev, EventHalted) != 0 {
			f.t.Fatalf("plan %s with %d halted events, want no halt", got.Status,
				countKind(ev, EventHalted))
		}
		return
	}
	if got.Status != StatusHalted || countKind(ev, EventHalted) != 1 {
		f.t.Fatalf("plan %s with %d halted events, want halted once", got.Status,
			countKind(ev, EventHalted))
	}
	if why := f.haltedReason(id); !strings.Contains(why, ReasonInterruptedUnknown) {
		f.t.Fatalf("halt reason %q, want %s", why, ReasonInterruptedUnknown)
	}
	sent := f.notes.sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, ReasonInterruptedUnknown) {
		f.t.Fatalf("notifications %+v, want one naming %s", sent, ReasonInterruptedUnknown)
	}
}

// starting with no queue item is retried; starting with its pending item
// adopts that item (never a second one).
func TestResume_StartingRetriesOrAdoptsItsItem(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(fmt.Sprintf("orphan=%v", orphan), func(t *testing.T) {
			f := newEngineFixture(t, fmt.Sprintf("resume_starting_%v", orphan))
			p := f.approved(chain(3, 101))
			f.killDuringStep2(p, "approved")
			s2 := f.stepAt(p.ID, 2)
			qid := *s2.Runtime.QueueID
			f.exec1(`UPDATE sage.plan_step SET state = 'starting', queue_id = NULL
				WHERE id = $1`, s2.ID)
			if orphan {
				f.exec1(`UPDATE sage.action_queue SET status = 'pending', decided_by = NULL,
					decided_at = NULL WHERE id = $1`, qid)
			} else {
				f.exec1(`DELETE FROM sage.action_queue WHERE id = $1`, qid)
			}
			b := f.restart()
			f.tickWith(b)
			s2 = f.stepAt(p.ID, 2)
			if s2.Runtime.State != StateExecuted && s2.Runtime.State != StateJudging {
				t.Fatalf("step 2 %s (%s), want it run", s2.Runtime.State,
					s2.Runtime.StateReason)
			}
			if f.manual.callCount() != 2 || len(f.queueItems(p.ID)) != 2 {
				t.Fatalf("%d runs, %d queue items, want 2 and 2 (one for step 2)",
					f.manual.callCount(), len(f.queueItems(p.ID)))
			}
		})
	}
}

// queued, with a run of its SQL already in the ledger: something ran it
// outside the engine's record, so resume halts instead of running it.
func TestResume_QueuedWithARunInTheLedgerHalts(t *testing.T) {
	f := newEngineFixture(t, "resume_queued_ledger")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "approved")
	s2 := f.stepAt(p.ID, 2)
	f.exec1(`INSERT INTO sage.action_log (action_type, finding_id, sql_executed, outcome)
		VALUES ('create_index', $1, $2, 'success')`, *s2.FindingID, s2.ProposedSQL)
	b := f.restart()
	f.tickWith(b)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateHalted || f.manual.callCount() != 1 {
		t.Fatalf("step 2 %s after %d runs, want halted and never run", s.Runtime.State,
			f.manual.callCount())
	}
	f.checkHalt(p.ID, true)
}

// running whose ledger row failed is recorded failed (the plan then halts
// on the failure, as any failed step does).
func TestResume_RunningWithAFailedRunFails(t *testing.T) {
	f := newEngineFixture(t, "resume_running_failed")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "after")
	f.exec1(`UPDATE sage.action_log SET outcome = 'failed' WHERE sql_executed = $1`,
		f.stepAt(p.ID, 2).ProposedSQL)
	b := f.restart()
	f.tickWith(b)
	s2 := f.stepAt(p.ID, 2)
	if s2.Runtime.State != StateFailed || !strings.Contains(s2.Runtime.StateReason, "failed") ||
		f.manual.callCount() != 2 {
		t.Fatalf("step 2 %s (%s) after %d runs, want failed after 2", s2.Runtime.State,
			s2.Runtime.StateReason, f.manual.callCount())
	}
}

// executed moves to judging; reverting reads its revert back and ends
// reverted (the index is not in the catalog).
func TestResume_ExecutedJudgesAndRevertingFinishes(t *testing.T) {
	f := newEngineFixture(t, "resume_exec_revert")
	p := f.approved(chain(3, 101))
	f.keepStep1(p)
	f.tick() // step 2 runs to executed
	s2 := f.stepAt(p.ID, 2)
	if s2.Runtime.State != StateExecuted {
		t.Fatalf("step 2 %s, want executed", s2.Runtime.State)
	}
	b := f.restart()
	f.tickWith(b)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateJudging || f.manual.callCount() != 2 {
		t.Fatalf("step 2 %s after %d runs, want judging after 2", s.Runtime.State,
			f.manual.callCount())
	}
	f.exec1(`UPDATE sage.plan_step SET state = 'reverting', claimed_by = 'engine-b'
		WHERE id = $1`, s2.ID)
	f.exec1(`UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id = $1`,
		*s2.Runtime.ActionLogID)
	c := f.restart()
	f.tickWith(c)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateReverted {
		t.Fatalf("step 2 %s (%s), want reverted", s.Runtime.State, s.Runtime.StateReason)
	}
}

// failingResume fails every evidence read.
type failingResume struct{}

func (failingResume) evidence(context.Context, Plan, Step) (resumeEvidence, error) {
	return resumeEvidence{}, errors.New("read the ledger: canceling statement due to " +
		"statement timeout")
}

// Error propagation: a read that fails during resume leaves the step as
// it was (never moved forward on missing evidence) and is retried on the
// next tick.
func TestResume_FailedReadKeepsTheStepAndRetries(t *testing.T) {
	f := newEngineFixture(t, "resume_read_fails")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "after")
	b := f.restart()
	b.resume = failingResume{}
	err := b.Tick(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("tick: %v, want the read failure", err)
	}
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateRunning || !b.needsResume() {
		t.Fatalf("step 2 %s, resume pending %v: want running and resume retried",
			s.Runtime.State, b.needsResume())
	}
	b.resume = dbResume{pool: f.pool}
	f.tickWith(b)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateJudging || b.needsResume() {
		t.Fatalf("step 2 %s, resume pending %v: want judging and resume settled",
			s.Runtime.State, b.needsResume())
	}
}

// Nil/empty: with no plan in flight resume changes nothing and logs
// nothing.
func TestResume_NothingInFlightDoesNothing(t *testing.T) {
	f := newEngineFixture(t, "resume_empty")
	var logs atomic.Int32
	e, err := NewEngine(EngineConfig{Pool: f.pool, Executor: f.exec, Waits: f.waits,
		Verdicts: f.verdicts, Now: f.clock.Now, Owner: "engine-c", Database: "db1",
		LogFn: func(string, string, ...any) { logs.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := e.Resume(f.ctx)
	if err != nil || rep != (ResumeReport{}) || logs.Load() != 0 {
		t.Fatalf("empty database: %+v %v with %d logs, want nothing", rep, err, logs.Load())
	}
	p := f.approved(chain(2, 101)) // approved, nothing started
	before := len(f.events(p.ID))
	rep, err = e.Resume(f.ctx)
	if err != nil || rep != (ResumeReport{}) || logs.Load() != 0 ||
		len(f.events(p.ID)) != before {
		t.Fatalf("plan with nothing in flight: %+v %v, %d logs, %d new events", rep, err,
			logs.Load(), len(f.events(p.ID))-before)
	}
}

// Boundary: the old leader's claim one second before it expires is not
// taken; one second after, it is.
func TestResume_ClaimExpiryBoundary(t *testing.T) {
	f := newEngineFixture(t, "resume_claim_boundary")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "after")
	until := *f.stepAt(p.ID, 2).Runtime.ClaimedUntil
	f.manual.crash = nil
	b := f.newEngine("engine-b", f.leader)
	f.clock.now = until.Add(-time.Second)
	f.tickWith(b)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateRunning ||
		s.Runtime.ClaimedBy != "engine-a" || !b.needsResume() {
		t.Fatalf("1s before expiry: step 2 %s claimed by %s, resume pending %v: want "+
			"untouched and retried", s.Runtime.State, s.Runtime.ClaimedBy, b.needsResume())
	}
	f.clock.now = until.Add(time.Second)
	f.tickWith(b)
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateJudging ||
		s.Runtime.ClaimedBy != "engine-b" {
		t.Fatalf("1s after expiry: step 2 %s claimed by %s, want judging by engine-b",
			s.Runtime.State, s.Runtime.ClaimedBy)
	}
}

// Concurrency: the old leader wakes up after the new one took the step's
// claim and tries one transition while the new leader makes its own; the
// claim refuses the old leader every time (50 runs).
func TestResume_OldLeaderTransitionIsRefusedByTheClaim(t *testing.T) {
	f := newEngineFixture(t, "resume_old_leader")
	p := f.approved(chain(1, 101))
	f.tick()
	s := f.stepAt(p.ID, 1)
	if s.Runtime.State != StateExecuted {
		t.Fatalf("step 1 %s, want executed", s.Runtime.State)
	}
	old := f.engine
	f.clock.Add(DefaultClaimLease + time.Second)
	next := f.newEngine("engine-b", f.leader)
	for run := range 50 {
		f.exec1(`UPDATE sage.plan_step SET state = 'executed', claimed_by = 'engine-b',
			claimed_until = $2 WHERE id = $1`, s.ID, f.clock.Now().Add(time.Minute))
		var wg sync.WaitGroup
		var oldErr, newErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			oldErr = old.transition(f.ctx, p.ID, s.ID, StateExecuted, StateJudging,
				stepWrite{claim: true})
		}()
		go func() {
			defer wg.Done()
			newErr = next.transition(f.ctx, p.ID, s.ID, StateExecuted, StateJudging,
				stepWrite{claim: true})
		}()
		wg.Wait()
		got := f.stepAt(p.ID, 1)
		if !errors.Is(oldErr, ErrStepMoved) || newErr != nil ||
			got.Runtime.ClaimedBy != "engine-b" || got.Runtime.State != StateJudging {
			t.Fatalf("run %d: old %v, new %v, step %s claimed by %s: want the old leader "+
				"refused", run, oldErr, newErr, got.Runtime.State, got.Runtime.ClaimedBy)
		}
	}
}

// State transitions: the same engine losing and regaining leadership (no
// restart) resumes on its next leading tick. Without the follower tick in
// between nothing resumes: the step its own crash left running waits.
func TestResume_RegainingLeadershipResumesTheSameEngine(t *testing.T) {
	f := newEngineFixture(t, "resume_regain")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "after")
	f.manual.mu.Lock()
	f.manual.crash = nil
	f.manual.mu.Unlock()
	f.tick()
	if s := f.stepAt(p.ID, 2); s.Runtime.State != StateRunning {
		t.Fatalf("leading tick with resume settled: step 2 %s, want running", s.Runtime.State)
	}
	f.leader.Store(false)
	f.tick()
	if !f.engine.needsResume() {
		t.Fatal("a follower tick must make the next leading tick resume")
	}
	f.leader.Store(true)
	f.tick()
	s := f.stepAt(p.ID, 2)
	if s.Runtime.State != StateJudging || s.Runtime.ClaimedBy != "engine-a" ||
		f.manual.callCount() != 2 || f.engine.needsResume() {
		t.Fatalf("after regaining leadership step 2 %s claimed by %s after %d runs, resume "+
			"pending %v: want judging by engine-a after 2, settled", s.Runtime.State,
			s.Runtime.ClaimedBy, f.manual.callCount(), f.engine.needsResume())
	}
}

// A run of the same SQL that another plan's step recorded as its own is
// not this step's run: the queued step runs its item instead of halting.
func TestResume_AnotherStepsRunIsNotThisStepsRun(t *testing.T) {
	f := newEngineFixture(t, "resume_other_run")
	p := f.approved(chain(3, 101))
	f.killDuringStep2(p, "approved")
	s2 := f.stepAt(p.ID, 2)
	var actionID int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, outcome) VALUES ('create_index', $1, $2, 'success') RETURNING id`,
		*s2.FindingID, s2.ProposedSQL).Scan(&actionID); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	other := mustCreate(t, f.store, chain(1, 102))
	f.exec1(`UPDATE sage.plan_step SET action_log_id = $1 WHERE plan_id = $2`, actionID,
		other.ID)
	b := f.restart()
	f.tickWith(b)
	s := f.stepAt(p.ID, 2)
	if (s.Runtime.State != StateExecuted && s.Runtime.State != StateJudging) ||
		f.manual.callCount() != 2 || *s.Runtime.ActionLogID == actionID {
		t.Fatalf("step 2 %s (%s) after %d runs, want its own run (not action %d)",
			s.Runtime.State, s.Runtime.StateReason, f.manual.callCount(), actionID)
	}
	f.checkHalt(p.ID, false)
}
