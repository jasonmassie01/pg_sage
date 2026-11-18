package changeplan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// plan-02a review: each of the plan runner's two locks refuses on its
// own. The step is queued with its item and this engine's live claim, so
// only the lock under test can stop the run.

// queuedStep queues plan p's step 1 the way runStep does (its item, the
// step queued with this engine's claim) without running it.
func (f *engineFixture) queuedStep(p Plan) (Step, store.QueuedAction) {
	f.t.Helper()
	s := p.Steps[0]
	if err := f.engine.transition(f.ctx, p.ID, s.ID, StatePending, StateStarting,
		stepWrite{claim: true, attempt: true}); err != nil {
		f.t.Fatalf("start step: %v", err)
	}
	queueID, err := f.engine.queue(f.ctx, p, s)
	if err != nil {
		f.t.Fatalf("queue step: %v", err)
	}
	qid := int64(queueID)
	if err := f.engine.transition(f.ctx, p.ID, s.ID, StateStarting, StateQueued,
		stepWrite{queueID: &qid, claim: true}); err != nil {
		f.t.Fatalf("queue the step: %v", err)
	}
	item, err := store.NewActionStore(f.pool).GetByID(f.ctx, queueID)
	if err != nil {
		f.t.Fatalf("read queue item %d: %v", queueID, err)
	}
	return s, *item
}

func (f *engineFixture) stepState(p Plan) StepState {
	f.t.Helper()
	return f.get(p.ID).Steps[0].Runtime.State
}

// The context marker: without it (an Actions approval, a card, the
// scheduled runner), or naming another step or engine, nothing runs even
// though the step is queued with this item and claimed.
func TestPlanRunner_MarkerLockRefusesOnItsOwn(t *testing.T) {
	f := newEngineFixture(t, "engine_runner_marker")
	runner := f.engine.Runner(f.manual)
	for _, c := range []struct {
		name string
		ctx  func(p Plan, s Step) context.Context
	}{
		{"no marker", func(Plan, Step) context.Context { return context.Background() }},
		{"another step", func(p Plan, s Step) context.Context {
			return context.WithValue(f.ctx, engineRunKey{},
				engineRun{plan: p.ID, step: s.ID + 1000, owner: "engine-a"})
		}},
		{"another engine", func(p Plan, s Step) context.Context {
			return context.WithValue(f.ctx, engineRunKey{},
				engineRun{plan: p.ID, step: s.ID, owner: "engine-b"})
		}},
	} {
		p := f.approved(chain(1, 101))
		s, item := f.queuedStep(p)
		_, err := runner.RunApproved(c.ctx(p, s), item, 7)
		if !errors.Is(err, ErrNotEngineRun) || f.manual.callCount() != 0 ||
			f.stepState(p) != StateQueued {
			t.Fatalf("%s: err %v, runs %d, step %s: want ErrNotEngineRun, none, queued",
				c.name, err, f.manual.callCount(), f.stepState(p))
		}
	}
}

// The compare-and-set: with the engine's marker, a step whose item, SQL or
// claim does not match is refused; when all match, the step runs once.
func TestPlanRunner_StepLockRefusesOnItsOwn(t *testing.T) {
	f := newEngineFixture(t, "engine_runner_cas")
	runner := f.engine.Runner(f.manual)
	marked := func(p Plan, s Step) context.Context {
		return context.WithValue(f.ctx, engineRunKey{},
			engineRun{plan: p.ID, step: s.ID, owner: "engine-a"})
	}
	for _, c := range []struct {
		name  string
		alter func(p Plan, s Step, item *store.QueuedAction)
	}{
		{"another queue item", func(_ Plan, _ Step, item *store.QueuedAction) {
			item.ID += 1000
		}},
		{"other SQL", func(_ Plan, _ Step, item *store.QueuedAction) {
			item.ProposedSQL = "DROP TABLE public.orders"
		}},
		{"lapsed claim", func(Plan, Step, *store.QueuedAction) {
			f.clock.Add(DefaultClaimLease + time.Second)
		}},
		{"claimed by another engine", func(p Plan, s Step, _ *store.QueuedAction) {
			f.exec1(`UPDATE sage.plan_step SET claimed_by = 'engine-b' WHERE id = $1`, s.ID)
		}},
		{"step not queued", func(p Plan, s Step, _ *store.QueuedAction) {
			f.exec1(`UPDATE sage.plan_step SET state = 'pending' WHERE id = $1`, s.ID)
		}},
	} {
		p := f.approved(chain(1, 101))
		s, item := f.queuedStep(p)
		before := f.stepState(p)
		c.alter(p, s, &item)
		if before != StateQueued {
			t.Fatalf("%s: step %s before the run, want queued", c.name, before)
		}
		_, err := runner.RunApproved(marked(p, s), item, 7)
		if !errors.Is(err, ErrNotEngineRun) || f.manual.callCount() != 0 ||
			f.stepState(p) == StateRunning {
			t.Fatalf("%s: err %v, runs %d, step %s: want ErrNotEngineRun and no run",
				c.name, err, f.manual.callCount(), f.stepState(p))
		}
	}
	p := f.approved(chain(1, 102))
	s, item := f.queuedStep(p)
	run, err := runner.RunApproved(marked(p, s), item, 7)
	if err != nil || run.ActionLogID <= 0 || f.manual.callCount() != 1 ||
		f.stepState(p) != StateRunning {
		t.Fatalf("matching run: %+v %v, runs %d, step %s: want one run, step running", run,
			err, f.manual.callCount(), f.stepState(p))
	}
}

func (f *engineFixture) exec1(sql string, args ...any) {
	f.t.Helper()
	tag, err := f.pool.Exec(f.ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("%s: %v (%d rows)", sql, err, tag.RowsAffected())
	}
}
