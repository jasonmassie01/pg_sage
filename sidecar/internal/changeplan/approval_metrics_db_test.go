package changeplan

import (
	"context"
	"errors"
	"testing"
)

// ux-03 on PostgreSQL: a new approval counts once by its stored mode; the
// same decision again (the live approval) and a refused one count nothing;
// up_to the last step is stored, and counted, as all.

func TestApproveWith_CountsNewApprovalsByStoredMode(t *testing.T) {
	pool := planPool(t, "plan_approval_metrics")
	s := NewStore(pool)
	ctx := context.Background()
	resetApprovalCounts()
	p := mustCreate(t, s, threeStepPlan())
	if _, err := s.ApproveWith(ctx, p.ID, Decision{PlanHash: "stale", Mode: ModeAll,
		Via: ViaUI}, 7); !errors.Is(err, ErrStaleHash) {
		t.Fatalf("stale approval: %v, want ErrStaleHash", err)
	}
	upTo := Decision{PlanHash: p.PlanHash, Mode: ModeUpTo, UpTo: 1, Via: ViaUI}
	for range 2 {
		if _, err := s.ApproveWith(ctx, p.ID, upTo, 7); err != nil {
			t.Fatalf("approve up to 1: %v", err)
		}
	}
	last := Decision{PlanHash: p.PlanHash, Mode: ModeUpTo, UpTo: 3, Via: ViaUI}
	if _, err := s.ApproveWith(ctx, p.ID, last, 7); err != nil {
		t.Fatalf("approve up to the last step: %v", err)
	}
	got := ApprovalCounts()
	if got[ModeUpTo] != 1 || got[ModeAll] != 1 || got[ModeStep] != 0 {
		t.Fatalf("ApprovalCounts = %v, want up_to 1 (repeat not counted), all 1, step 0", got)
	}
}
