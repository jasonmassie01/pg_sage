package changeplan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// plan-02c: a revert left reverting resumes on a later tick, a rollback in
// progress outside the plan is waited on, the emergency stop holds a walk
// between steps, and the read-back covers each kind of rollback.

// State transitions: a step left reverting (a restart after the
// transition, before its rollback) is rolled back on a later tick; while
// a rollback runs outside the plan it stays reverting and waits.
func TestRevertTo_ResumesAStepLeftReverting(t *testing.T) {
	f := newEngineFixture(t, "rollback_resume")
	p := f.approved(chain(2, 101))
	mid := f.runVerdicts(p.ID, nil, 3) // step 1 kept
	s1 := mid.Steps[0]
	if s1.Runtime.State != StateKept || mid.Steps[1].Runtime.State != StatePending {
		t.Fatalf("steps %v, want step 1 kept and step 2 pending", states(mid))
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.plan_step SET state = 'reverting',
		claimed_by = NULL WHERE id = %d;
		UPDATE sage.action_log SET outcome = 'rolling_back' WHERE id = %d`,
		s1.ID, *s1.Runtime.ActionLogID))
	f.tick()
	got := f.get(p.ID).Steps[0].Runtime
	if got.State != StateReverting || f.exec.rollbackCount() != 0 ||
		!strings.Contains(got.StateReason, "in progress outside the plan") {
		t.Fatalf("rolling back outside: step 1 %+v, %d rollbacks: want reverting, waiting",
			got, f.exec.rollbackCount())
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'monitoring'
		WHERE id = %d`, *s1.Runtime.ActionLogID))
	f.tick()
	got = f.get(p.ID).Steps[0].Runtime
	if got.State != StateReverted || f.exec.rollbackCount() != 1 ||
		!strings.Contains(got.StateReason, "reverted by the plan") {
		t.Fatalf("resumed: step 1 %+v, %d rollbacks: want reverted by one rollback", got,
			f.exec.rollbackCount())
	}
}

// The emergency stop set during a walk holds it after the revert in
// flight: nothing else is rolled back until the stop is cleared, then the
// walk goes on where it stopped.
func TestRevertTo_EmergencyStopHoldsTheWalk(t *testing.T) {
	f := newEngineFixture(t, "rollback_stop")
	p := f.approved(walkChain(3, 3))
	f.exec.afterRollback = func() { f.stopped.Store(true) }
	f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 12)
	for range 3 {
		f.tick()
	}
	got := f.get(p.ID)
	if !slices.Equal(states(got), []StepState{StateKept, StateKept, StateReverted}) ||
		got.Status != StatusRunning || got.PauseReason != pauseEmergencyStop {
		t.Fatalf("under the stop: plan %s (%q) steps %v, want running, paused, only 3 "+
			"reverted", got.Status, got.PauseReason, states(got))
	}
	f.exec.afterRollback = nil
	f.stopped.Store(false)
	got = f.runVerdicts(p.ID, nil, 5)
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3, 2, 1}) ||
		got.Status != StatusHalted {
		t.Fatalf("after the stop: plan %s rolled back %v, want halted with [3 2 1]",
			got.Status, rb)
	}
}

// The read-back of each kind of rollback, on the fixture's catalog. An
// INVALID index (a failed CONCURRENTLY build) is not reproduced here: the
// valid flag is read in the same query as its presence.
func TestReadBack_EachKindOfRollback(t *testing.T) {
	f := newEngineFixture(t, "rollback_readback")
	mustExecPlan(t, f, `CREATE TABLE public.rb (a int);
		CREATE INDEX rb_a ON public.rb (a)`)
	var prior string
	if err := f.pool.QueryRow(f.ctx, `SELECT setting FROM pg_settings
		WHERE name = 'random_page_cost'`).Scan(&prior); err != nil {
		t.Fatalf("read random_page_cost: %v", err)
	}
	other := prior + "1"
	for _, c := range []struct {
		sql    string
		prior  *string
		landed bool
		note   string
	}{
		{"DROP INDEX CONCURRENTLY public.rb_a", nil, false, "still there"},
		{"DROP INDEX CONCURRENTLY IF EXISTS public.rb_gone", nil, true, "is gone"},
		{"drop index rb_gone;", nil, true, "public.rb_gone is gone"},
		{"CREATE INDEX CONCURRENTLY rb_a ON public.rb (a)", nil, true, "there and valid"},
		{"CREATE INDEX CONCURRENTLY rb_b ON public.rb (a)", nil, false, "present false"},
		{"REINDEX INDEX CONCURRENTLY public.rb_a", nil, true, "there and valid"},
		{"DROP INDEX public.rb_a, public.rb_b", nil, false, "no read-back"},
		{"", nil, false, "no read-back"},
		{"ALTER SYSTEM RESET work_mem", nil, false, "no prior value"},
		{"ALTER SYSTEM RESET random_page_cost", &prior, true, "back at"},
	} {
		landed, note, err := f.engine.readBack(f.ctx, c.sql, c.prior)
		if err != nil || landed != c.landed || !strings.Contains(note, c.note) {
			t.Errorf("readBack(%q) = (%v, %q, %v), want (%v, ...%q...)", c.sql, landed, note,
				err, c.landed, c.note)
		}
	}
	// Boundary: a setting that is not back at its prior after every retry.
	landed, note, err := f.engine.readBack(f.ctx, "ALTER SYSTEM RESET random_page_cost",
		&other)
	if err != nil || landed || !strings.Contains(note, "not its prior") {
		t.Fatalf("setting off its prior: (%v, %q, %v), want not landed", landed, note, err)
	}
}

// readLanding: a rollback in progress is pending, an action not rolled back
// has not landed, and a missing action is an error naming it.
func TestReadLanding_OutcomesAndErrors(t *testing.T) {
	f := newEngineFixture(t, "rollback_landing")
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed, rollback_sql, outcome, executed_at) VALUES ('create_index',
		'CREATE INDEX CONCURRENTLY x ON public.t (a)', 'DROP INDEX CONCURRENTLY public.x',
		'rolling_back', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert an action: %v", err)
	}
	if l, err := f.engine.readLanding(f.ctx, id); err != nil || !l.pending || l.landed {
		t.Fatalf("rolling_back: %+v (%v), want pending", l, err)
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'monitoring'
		WHERE id = %d`, id))
	if l, err := f.engine.readLanding(f.ctx, id); err != nil || l.pending || l.landed ||
		!strings.Contains(l.note, `"monitoring"`) {
		t.Fatalf("monitoring: %+v (%v), want not landed, naming the outcome", l, err)
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'rolled_back'
		WHERE id = %d`, id))
	if l, err := f.engine.readLanding(f.ctx, id); err != nil || !l.landed {
		t.Fatalf("rolled back, index absent: %+v (%v), want landed", l, err)
	}
	if _, err := f.engine.readLanding(f.ctx, 987654); !errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "987654") {
		t.Fatalf("missing action: %v, want ErrNotFound naming it", err)
	}
}

// Error propagation: a read that fails (the step's action row is gone)
// leaves the step reverting with the error and rolls nothing back; it is
// never recorded reverted or failed on a read it could not make.
func TestRevertTo_ReadFailureLeavesTheStepReverting(t *testing.T) {
	f := newEngineFixture(t, "rollback_readfail")
	p := f.approved(chain(1, 101))
	mid := f.runVerdicts(p.ID, nil, 2) // step 1 judging
	s := mid.Steps[0]
	if s.Runtime.State != StateJudging {
		t.Fatalf("steps %v, want step 1 judging", states(mid))
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.plan_step SET state = 'reverting',
		action_log_id = %d WHERE id = %d`, *s.Runtime.ActionLogID+1000, s.ID))
	f.tick()
	got := f.get(p.ID).Steps[0].Runtime
	if got.State != StateReverting || f.exec.rollbackCount() != 0 ||
		!strings.Contains(got.StateReason, "read the action before its revert") {
		t.Fatalf("step %+v, %d rollbacks: want reverting with the read error", got,
			f.exec.rollbackCount())
	}
}
