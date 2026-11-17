package changeplan

import (
	"errors"
	"testing"
)

// plan-03b review: a chat card decides proposed plans only. The API checks
// the status before it consumes the card, but an approval in the app can
// land between that check and ApproveWith; ProposedOnly makes the store
// refuse then, so a chat card never supersedes the app's approval.

// No concurrent access test here: the refusal is decided under the plan
// row lock ApproveWith already takes (TestApproveWith_Concurrent* cover it).

func TestApproveWith_ProposedOnlyRefusesAnApprovedPlan(t *testing.T) {
	f := newEngineFixture(t, "proposed_only")
	p := mustCreate(t, f.store, threeStepPlan())
	first, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, ModeAll, 0), 9)
	if err != nil {
		t.Fatalf("approve all: %v", err)
	}
	chat := Decision{PlanHash: p.PlanHash, Mode: ModeUpTo, UpTo: 1, Via: ViaChat,
		ProposedOnly: true}
	_, err = f.store.ApproveWith(f.ctx, p.ID, chat, 10)
	if !errors.Is(err, ErrNotApprovable) {
		t.Fatalf("proposed-only approval of an approved plan: %v, want ErrNotApprovable", err)
	}
	got, err := f.store.Approvals(f.ctx, p.ID)
	if err != nil || len(got) != 1 || got[0].ID != first.ID || got[0].Mode != ModeAll ||
		got[0].VoidedAt != nil {
		t.Fatalf("approvals %+v (%v), want the first approve-all kept live", got, err)
	}
}

func TestApproveWith_ProposedOnlyApprovesAProposedPlan(t *testing.T) {
	f := newEngineFixture(t, "proposed_only_ok")
	p := mustCreate(t, f.store, threeStepPlan())
	chat := Decision{PlanHash: p.PlanHash, Mode: ModeUpTo, UpTo: 1, Via: ViaChat,
		ProposedOnly: true}
	a, err := f.store.ApproveWith(f.ctx, p.ID, chat, 10)
	if err != nil {
		t.Fatalf("proposed-only approval of a proposed plan: %v", err)
	}
	if a.Mode != ModeUpTo || a.UpTo == nil || *a.UpTo != 1 || a.Via != ViaChat ||
		a.ApproverUserID != 10 {
		t.Fatalf("approval %+v, want up_to 1 via chat by user 10", a)
	}
}
