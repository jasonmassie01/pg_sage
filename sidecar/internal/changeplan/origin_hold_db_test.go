package changeplan

import (
	"strings"
	"testing"
	"time"
)

// review-03b on the engine path (holdBeforeStart -> coverageNow): under
// approve-all, a step of a model-origin plan that the model proposed (or
// that has no origin row: fail closed) stops at awaiting_approval with the
// model_origin reason and is never queued, while a generator step of the
// same plan runs at its class's trust. A live model authority (G-M1) lets
// approve-all cover the model's steps. No concurrent test here: the hold
// is read inside the engine's locked tick, which caps_db_test exercises.

// modelPlan is chain(2) proposed by model m1.
func modelPlan() Plan {
	p := chain(2, 101)
	p.Origin, p.OriginRef, p.Model = OriginModel, "review_run:1", "m1"
	return p
}

// setOrigin records who proposed step ordinal of plan p.
func (f *engineFixture) setOrigin(p Plan, ordinal int, origin, ref string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.plan_step_origin (step_id, plan_id,
		origin, origin_ref) SELECT id, plan_id, $3, $4 FROM sage.plan_step
		WHERE plan_id = $1 AND ordinal = $2`, p.ID, ordinal, origin, ref); err != nil {
		f.t.Fatalf("record the origin of step %d: %v", ordinal, err)
	}
}

func TestModelHoldEngine_ModelStepAwaitsItsOwnApproval(t *testing.T) {
	f := defaultCaps(t, "model_hold")
	p := f.approved(modelPlan())
	f.setOrigin(p, 1, "generator", "indexes")
	f.setOrigin(p, 2, "model", "m1")
	got := f.runUntil(p.ID, "improved", 6)
	for range 3 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	got = f.get(p.ID)
	s1, s2 := got.Steps[0].Runtime, got.Steps[1].Runtime
	if s1.State != StateKept || s2.State != StateAwaitingApproval ||
		!strings.HasPrefix(s2.StateReason, ReasonModelOrigin+":") ||
		!strings.Contains(s2.StateReason, "m1") {
		t.Fatalf("steps %+v / %+v, want the generator step kept and the model step "+
			"awaiting_approval (model_origin, m1)", s1, s2)
	}
	if f.manual.callCount() != 1 || len(f.queueItems(p.ID)) != 1 {
		t.Fatalf("%d runs, %d queue items: want only the generator step run",
			f.manual.callCount(), len(f.queueItems(p.ID)))
	}
}

func TestModelHoldEngine_StepWithoutAnOriginRowIsHeld(t *testing.T) {
	f := defaultCaps(t, "model_hold_norow")
	p := f.approved(modelPlan())
	for range 4 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	s := f.get(p.ID).Steps[0].Runtime
	if s.State != StateAwaitingApproval || !strings.HasPrefix(s.StateReason,
		ReasonModelOrigin+":") || f.manual.callCount() != 0 || len(f.queueItems(p.ID)) != 0 {
		t.Fatalf("step %+v, %d runs: want awaiting_approval (model_origin) with nothing "+
			"queued", s, f.manual.callCount())
	}
}

func TestModelHoldEngine_GrantedModelFollowsTheClassLevel(t *testing.T) {
	f := defaultCaps(t, "model_hold_granted")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.review_model_authority
		(model, granted_by) VALUES ('m1', 'admin:7')`); err != nil {
		t.Fatalf("grant: %v", err)
	}
	p := f.approved(modelPlan())
	f.setOrigin(p, 1, "model", "m1")
	f.setOrigin(p, 2, "model", "m1")
	got := f.runUntil(p.ID, "improved", 12)
	if got.Status != StatusCompleted || f.manual.callCount() != 2 {
		t.Fatalf("plan %s after %d runs, want completed with both model steps run under "+
			"approve-all once m1 holds authority", got.Status, f.manual.callCount())
	}
}
