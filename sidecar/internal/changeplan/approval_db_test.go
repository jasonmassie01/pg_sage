package changeplan

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// plan-03a on a real server: the store records approval modes and step
// approvals, voids both on an edit, and decides reject, resume and cancel
// only from the statuses they apply to.

func decide3(hash string, mode Mode, upTo int) Decision {
	return Decision{PlanHash: hash, Mode: mode, UpTo: upTo, Via: ViaCLI}
}

func TestApproveWith_RecordsModeUpToAndChannel(t *testing.T) {
	f := newEngineFixture(t, "modes_record")
	p := mustCreate(t, f.store, threeStepPlan())
	a, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeUpTo, 2), 9)
	if err != nil {
		t.Fatalf("approve up to 2: %v", err)
	}
	if a.Mode != ModeUpTo || a.UpTo == nil || *a.UpTo != 2 || a.Via != ViaCLI ||
		a.ApproverUserID != 9 || a.PlanHash != p.PlanHash {
		t.Fatalf("approval %+v, want up_to 2 via cli by user 9", a)
	}
	got, err := f.store.Approvals(f.ctx, p.ID)
	if err != nil || len(got) != 1 || got[0].Mode != ModeUpTo || *got[0].UpTo != 2 ||
		got[0].Via != ViaCLI {
		t.Fatalf("stored approvals %+v (%v), want the up_to 2 approval", got, err)
	}
	if st := f.get(p.ID).Status; st != StatusApproved {
		t.Fatalf("plan status %s, want approved", st)
	}
	ev := f.events(p.ID)
	last := ev[len(ev)-1]
	if last.Kind != EventApproved || last.Detail["mode"] != "up_to" ||
		last.Detail["via"] != "cli" || last.Actor != "user:9" {
		t.Fatalf("approved event %+v, want mode up_to via cli by user:9", last)
	}
}

// Boundary: up_to equal to the step count is stored as all.
func TestApproveWith_UpToTheStepCountIsStoredAsAll(t *testing.T) {
	f := newEngineFixture(t, "modes_upto_all")
	p := mustCreate(t, f.store, threeStepPlan())
	a, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeUpTo, 3), 9)
	if err != nil || a.Mode != ModeAll || a.UpTo != nil {
		t.Fatalf("approve up to 3 of 3 = %+v (%v), want mode all", a, err)
	}
}

func TestApproveWith_RefusesInvalidDecisionsAndStoresNothing(t *testing.T) {
	f := newEngineFixture(t, "modes_invalid")
	p := mustCreate(t, f.store, threeStepPlan())
	cases := map[string]struct {
		d    Decision
		code string
		want error
	}{
		"up_to past the steps": {decide3(p.PlanHash, ModeUpTo, 4), CodeInvalidUpTo, nil},
		"unknown mode":         {decide3(p.PlanHash, "most", 0), CodeInvalidMode, nil},
		"missing hash":         {decide3("", ModeAll, 0), CodeHashRequired, nil},
		"stale hash":           {decide3(threeStepHashOther(), ModeAll, 0), "", ErrStaleHash},
	}
	for name, c := range cases {
		_, err := f.store.ApproveWith(f.ctx, p.ID, c.d, 9)
		switch {
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", name, err, c.want)
		case c.want == nil && decisionCode(err) != c.code:
			t.Errorf("%s: %v, want code %s", name, err, c.code)
		}
	}
	_, err := f.store.ApproveWith(f.ctx, 999999, decide3(p.PlanHash, ModeAll, 0), 9)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("approve a missing plan: %v, want ErrNotFound", err)
	}
	if got, err := f.store.Approvals(f.ctx, p.ID); err != nil || len(got) != 0 {
		t.Fatalf("approvals after refusals %+v (%v), want none", got, err)
	}
}

func threeStepHashOther() string {
	p := threeStepPlan()
	p.Goal = "another goal"
	return PlanHash(p)
}

// Concurrency: operators approving one plan with the same hash and mode at
// once create one live approval, and every one of them gets it back.
func TestApproveWith_ConcurrentSameDecisionIsOneLiveApproval(t *testing.T) {
	f := newEngineFixture(t, "modes_concurrent")
	p := mustCreate(t, f.store, threeStepPlan())
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeAll, 0), 7+i)
			ids[i], errs[i] = a.ID, err
		}()
	}
	wg.Wait()
	for i := range 8 {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("approval %d: id %d (%v), want %d", i, ids[i], errs[i], ids[0])
		}
	}
	var live int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.plan_approval
		WHERE plan_id = $1 AND voided_at IS NULL`, p.ID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("%d live approvals (%v), want 1", live, err)
	}
	if n := countKind(f.events(p.ID), EventApproved); n != 1 {
		t.Fatalf("%d approved events, want 1", n)
	}
}

// State transition: a new mode at the same hash supersedes the live one.
func TestApproveWith_AnotherModeSupersedesTheLiveApproval(t *testing.T) {
	f := newEngineFixture(t, "modes_supersede")
	p := mustCreate(t, f.store, threeStepPlan())
	first, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeAll, 0), 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeStep, 0), 10)
	if err != nil || second.ID == first.ID || second.Mode != ModeStep {
		t.Fatalf("second approval %+v (%v), want a new step approval", second, err)
	}
	got, _ := f.store.Approvals(f.ctx, p.ID)
	if len(got) != 2 || got[0].VoidedAt == nil || got[0].VoidReason != "superseded" ||
		got[1].VoidedAt != nil {
		t.Fatalf("approvals %+v, want the first superseded and the second live", got)
	}
}

func stepByStep(t *testing.T, f *engineFixture, in Plan) Plan {
	t.Helper()
	p := mustCreate(t, f.store, in)
	if _, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeStep, 0), 9); err != nil {
		t.Fatalf("approve step by step: %v", err)
	}
	return f.get(p.ID)
}

func TestApproveStep_RecordsOnceAndRefusesBadInput(t *testing.T) {
	f := newEngineFixture(t, "step_approve")
	p := stepByStep(t, f, threeStepPlan())
	other := mustCreate(t, f.store, chain(1, 101))
	sa, err := f.store.ApproveStep(f.ctx, p.ID, p.Steps[0].ID, p.PlanHash, 11, ViaAPI)
	if err != nil || sa.StepID != p.Steps[0].ID || sa.Ordinal != 1 || sa.ApproverUserID != 11 ||
		sa.Via != ViaAPI || sa.PlanHash != p.PlanHash {
		t.Fatalf("step approval %+v (%v), want step 1 by user 11", sa, err)
	}
	again, err := f.store.ApproveStep(f.ctx, p.ID, p.Steps[0].ID, p.PlanHash, 12, ViaUI)
	if err != nil || again.ID != sa.ID {
		t.Fatalf("approving step 1 again = %+v (%v), want the live approval %d", again, err,
			sa.ID)
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, other.Steps[0].ID, p.PlanHash, 11,
		ViaAPI); decisionCode(err) != CodeStepNotInPlan {
		t.Errorf("another plan's step: %v, want %s", err, CodeStepNotInPlan)
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, p.Steps[1].ID, threeStepHashOther(), 11,
		ViaAPI); !errors.Is(err, ErrStaleHash) {
		t.Errorf("stale hash: %v, want ErrStaleHash", err)
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, p.Steps[1].ID, p.PlanHash, 11,
		"fax"); decisionCode(err) != CodeInvalidVia {
		t.Errorf("unknown channel: %v, want %s", err, CodeInvalidVia)
	}
	got, err := f.store.StepApprovals(f.ctx, p.ID)
	if err != nil || len(got) != 1 || got[0].ID != sa.ID {
		t.Fatalf("step approvals %+v (%v), want only step 1's", got, err)
	}
	d, err := f.store.Detail(f.ctx, p.ID)
	if err != nil || len(d.Coverage) != 3 || !d.Coverage[0].Covered ||
		d.Coverage[0].By != CoveredByStep || d.Coverage[1].Covered ||
		len(d.Approvals) != 1 || len(d.StepApprovals) != 1 || d.Plan.ID != p.ID {
		t.Fatalf("detail %+v (%v), want step 1 covered by its approval only", d, err)
	}
}

// State transition: an edit voids the plan approval and every step
// approval, and the plan goes back to proposed with nothing covered.
func TestEdit_VoidsPlanAndStepApprovals(t *testing.T) {
	f := newEngineFixture(t, "step_edit_voids")
	p := stepByStep(t, f, threeStepPlan())
	for _, s := range p.Steps[:2] {
		if _, err := f.store.ApproveStep(f.ctx, p.ID, s.ID, p.PlanHash, 11, ViaAPI); err != nil {
			t.Fatal(err)
		}
	}
	edited := p.Steps[0]
	edited.DeadlineMinutes = 120
	if _, err := f.store.UpdateStep(f.ctx, p.ID, edited, newFakeResolver()); err != nil {
		t.Fatalf("edit: %v", err)
	}
	steps, _ := f.store.StepApprovals(f.ctx, p.ID)
	for _, sa := range steps {
		if sa.VoidedAt == nil || sa.VoidReason != VoidEdited {
			t.Errorf("step approval %+v survived the edit", sa)
		}
	}
	d, err := f.store.Detail(f.ctx, p.ID)
	if err != nil || d.Plan.Status != StatusProposed || len(coveredOrdinals(d.Coverage)) != 0 {
		t.Fatalf("after the edit: %+v (%v), want proposed with nothing covered", d, err)
	}
	if len(steps) != 2 {
		t.Fatalf("%d step approvals, want 2", len(steps))
	}
}

// Concurrency: one operator approving while another edits never leaves a
// live approval on a changed plan.
func TestApproveWhileEditing_NeverLeavesALiveApprovalOnAChangedPlan(t *testing.T) {
	f := newEngineFixture(t, "approve_vs_edit")
	for round := range 4 {
		p := mustCreate(t, f.store, chain(2, int64(101+round%2)))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeAll, 0), 9)
		}()
		go func() {
			defer wg.Done()
			s := p.Steps[1]
			s.DeadlineMinutes = 90 + round
			_, _ = f.store.UpdateStep(f.ctx, p.ID, s, newFakeResolver())
		}()
		wg.Wait()
		if err := f.store.Verify(f.ctx, p.ID); err != nil && !errors.Is(err, ErrNotApproved) {
			t.Fatalf("round %d: %v, want approved at the current hash or not approved",
				round, err)
		}
	}
}

func TestReject_OnlyFromProposedWithAReason(t *testing.T) {
	f := newEngineFixture(t, "reject")
	p := mustCreate(t, f.store, threeStepPlan())
	if err := f.store.Reject(f.ctx, p.ID, "  ", 9); decisionCode(err) != CodeReasonRequired {
		t.Fatalf("blank reason: %v, want %s", err, CodeReasonRequired)
	}
	if err := f.store.Reject(f.ctx, p.ID, "the index is on the wrong column", 9); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if st := f.get(p.ID).Status; st != StatusRejected {
		t.Fatalf("status %s, want rejected", st)
	}
	ev := f.events(p.ID)
	if last := ev[len(ev)-1]; last.Kind != EventRejected || last.Actor != "user:9" ||
		last.Detail["reason"] != "the index is on the wrong column" {
		t.Fatalf("rejected event %+v", last)
	}
	if err := f.store.Reject(f.ctx, p.ID, "again", 9); !errors.Is(err, ErrNotRejectable) {
		t.Fatalf("reject a rejected plan: %v, want ErrNotRejectable", err)
	}
	approved := f.approved(chain(1, 102))
	if err := f.store.Reject(f.ctx, approved.ID, "no", 9); !errors.Is(err, ErrNotRejectable) {
		t.Fatalf("reject an approved plan: %v, want ErrNotRejectable", err)
	}
}

// Cancel ends an approved or halted plan and voids its approvals; a
// proposed or running plan is refused.
func TestCancel_FromApprovedOrHaltedNeverRunning(t *testing.T) {
	f := newEngineFixture(t, "cancel")
	proposed := mustCreate(t, f.store, threeStepPlan())
	if err := f.store.Cancel(f.ctx, proposed.ID, "", 9); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel a proposed plan: %v, want ErrNotCancellable", err)
	}
	approved := f.approved(chain(1, 101))
	if err := f.store.Cancel(f.ctx, approved.ID, "not this week", 9); err != nil {
		t.Fatalf("cancel an approved plan: %v", err)
	}
	if st := f.get(approved.ID).Status; st != StatusCancelled {
		t.Fatalf("status %s, want cancelled", st)
	}
	if err := f.store.Verify(f.ctx, approved.ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("a cancelled plan's approval: %v, want voided", err)
	}
	running := f.approved(chain(2, 102))
	f.tick()
	if st := f.get(running.ID).Status; st != StatusRunning {
		t.Fatalf("plan %s after a tick, want running", st)
	}
	if err := f.store.Cancel(f.ctx, running.ID, "", 9); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel a running plan: %v, want ErrNotCancellable", err)
	}
	halted := failFirst(t, f, independent(2, 101))
	if err := f.store.Cancel(f.ctx, halted.ID, "", 9); err != nil {
		t.Fatalf("cancel a halted plan: %v", err)
	}
	if st := f.get(halted.ID).Status; st != StatusCancelled {
		t.Fatalf("status %s, want cancelled", st)
	}
}

// independent is n index steps on target with no edges.
func independent(n int, target int64) Plan {
	p := chain(n, target)
	p.Edges = nil
	return p
}

// failFirst approves p, refuses its first step's SQL and ticks until the
// plan halts.
func failFirst(t *testing.T, f *engineFixture, in Plan) Plan {
	t.Helper()
	p := f.approved(in)
	f.manual.mu.Lock()
	f.manual.refuse[p.Steps[0].ProposedSQL] = "test refusal"
	f.manual.mu.Unlock()
	got := f.runUntil(p.ID, "improved", 6)
	if got.Status != StatusHalted || got.Steps[0].Runtime.State != StateFailed {
		t.Fatalf("plan %s, step 1 %s: want halted on a failed step 1", got.Status,
			got.Steps[0].Runtime.State)
	}
	f.manual.mu.Lock()
	delete(f.manual.refuse, p.Steps[0].ProposedSQL)
	f.manual.mu.Unlock()
	return got
}

// Resume only from halted, at the current hash: the failed step is
// skipped with the resume marked, and the plan goes on to completion.
func TestResume_OnlyFromHaltedThenThePlanGoesOn(t *testing.T) {
	f := newEngineFixture(t, "resume")
	proposed := mustCreate(t, f.store, threeStepPlan())
	if err := f.store.Resume(f.ctx, proposed.ID, proposed.PlanHash, 9); !errors.Is(err,
		ErrNotResumable) {
		t.Fatalf("resume a proposed plan: %v, want ErrNotResumable", err)
	}
	p := failFirst(t, f, independent(2, 101))
	if err := f.store.Resume(f.ctx, p.ID, threeStepHashOther(), 9); !errors.Is(err,
		ErrStaleHash) {
		t.Fatalf("resume at a stale hash: %v, want ErrStaleHash", err)
	}
	if err := f.store.Resume(f.ctx, p.ID, "", 9); decisionCode(err) != CodeHashRequired {
		t.Fatalf("resume without a hash: %v, want %s", err, CodeHashRequired)
	}
	if err := f.store.Resume(f.ctx, p.ID, p.PlanHash, 9); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := f.get(p.ID)
	if got.Status != StatusRunning || got.Steps[0].Runtime.State != StateSkipped ||
		got.Steps[0].Runtime.ResumedAt == nil {
		t.Fatalf("after resume: %s, step 1 %+v; want running, step 1 skipped and resumed",
			got.Status, got.Steps[0].Runtime)
	}
	ev := f.events(p.ID)
	if last := ev[len(ev)-1]; last.Kind != EventResumed || last.Actor != "user:9" {
		t.Fatalf("resumed event %+v", last)
	}
	if err := f.store.Resume(f.ctx, p.ID, p.PlanHash, 9); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("resume a running plan: %v, want ErrNotResumable", err)
	}
	done := f.runUntil(p.ID, "improved", 8)
	if done.Status != StatusCompleted || done.Steps[1].Runtime.State != StateKept {
		t.Fatalf("plan %s, step 2 %s: want completed with step 2 kept", done.Status,
			done.Steps[1].Runtime.State)
	}
}

// Error propagation: a store on a closed pool says what it was doing.
func TestApprovalStore_ClosedPoolErrorsNameTheOperation(t *testing.T) {
	f := newEngineFixture(t, "closed_pool")
	p := mustCreate(t, f.store, threeStepPlan())
	f.pool.Close()
	ctx := context.Background()
	if _, err := f.store.Detail(ctx, p.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("detail on a closed pool: %v", err)
	}
	if _, err := f.store.ApproveStep(ctx, p.ID, p.Steps[0].ID, p.PlanHash, 9,
		ViaAPI); err == nil {
		t.Error("step approval on a closed pool succeeded")
	}
	if err := f.store.Cancel(ctx, p.ID, "", 9); err == nil {
		t.Error("cancel on a closed pool succeeded")
	}
}
