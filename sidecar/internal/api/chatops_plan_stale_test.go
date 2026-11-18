package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Stale plan cards (plan-03b review): what a card offered, the hash it
// showed and the plan's status are all checked again when a button is
// pressed, so a card that no longer matches the plan decides nothing and
// stays unused.

// TestChatPlanTwoClicksMakeOneDecision covers two presses of one card;
// the race with an app approval is simulated below.

func TestChatPlanStaleUpToOfferIsRefused(t *testing.T) {
	cf := newChatCardFixture(t)
	// Step 3 is high tier, so chat may approve up to step 2 only; a card
	// claiming "up to step 3" (stale or forged) must not approve step 3.
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table), highStep(3))
	token := cf.issuePlan(t, cf.slackCh, p, 3, chatops.PlanApproveUpTo, chatops.PlanReject)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveUpToAction,
		token, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_offered" {
		t.Fatalf("up to step 3 past a high-tier step 3: %d %v", code, body)
	}
	if got := cf.planDetail(t, p.ID); got.Plan.Status != changeplan.StatusProposed ||
		len(got.Approvals) != 0 {
		t.Fatalf("a stale up-to card decided: %s %+v", got.Plan.Status, got.Approvals)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a refused up-to press used the card: %+v", card)
	}
}

func TestChatPlanRejectOfAChangedPlanIsRefused(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	step := p.Steps[1]
	step.DeadlineMinutes = 90
	if _, err := changeplan.NewStore(cf.pool).UpdateStep(context.Background(), p.ID, step,
		planResolver{}); err != nil {
		t.Fatalf("edit the plan: %v", err)
	}
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanRejectAction, token,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "content_changed" {
		t.Fatalf("reject from a stale card: %d %v", code, body)
	}
	if got := cf.planDetail(t, p.ID); got.Plan.Status != changeplan.StatusProposed {
		t.Fatalf("a stale card rejected the edited plan: %s", got.Plan.Status)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a stale reject used the card: %+v", card)
	}
}

func TestChatPlanRefusesAPlanAlreadyApprovedInTheApp(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	if _, err := changeplan.NewStore(cf.pool).ApproveWith(context.Background(), p.ID,
		changeplan.Decision{Mode: changeplan.ModeAll, PlanHash: p.PlanHash,
			Via: changeplan.ViaUI}, cf.operator); err != nil {
		t.Fatalf("approve in the app: %v", err)
	}
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveNextAction,
		token, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_pending" {
		t.Fatalf("chat press on an approved plan: %d %v", code, body)
	}
	d := cf.planDetail(t, p.ID)
	live := 0
	for _, a := range d.Approvals {
		if a.Via == changeplan.ViaChat {
			t.Fatalf("chat recorded an approval over the app's: %+v", d.Approvals)
		}
		if a.Mode == changeplan.ModeAll && a.Via == changeplan.ViaUI {
			live++
		}
	}
	if live != 1 || d.Plan.Status != changeplan.StatusApproved {
		t.Fatalf("the app's approve-all was not kept: %s %+v", d.Plan.Status, d.Approvals)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a refused press used the card: %+v", card)
	}
}

// appApprovesFirst is the real plan store where an approval in the app
// lands just before the chat decision: the race the status check before
// the card is consumed cannot close.
type appApprovesFirst struct {
	planService
	operator int
}

func (s appApprovesFirst) ApproveWith(ctx context.Context, id int64, d changeplan.Decision,
	userID int) (changeplan.Approval, error) {
	app := changeplan.Decision{Mode: changeplan.ModeAll, PlanHash: d.PlanHash,
		Via: changeplan.ViaUI}
	if _, err := s.planService.ApproveWith(ctx, id, app, s.operator); err != nil {
		return changeplan.Approval{}, err
	}
	return s.planService.ApproveWith(ctx, id, d, userID)
}

func TestChatPlanNeverSupersedesAnAppApprovalThatRacesIt(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	restore := chatPlanStores
	chatPlanStores = func(inst *fleet.DatabaseInstance) planService {
		return appApprovesFirst{planService: restore(inst), operator: cf.operator}
	}
	t.Cleanup(func() { chatPlanStores = restore })
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveNextAction,
		token, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_pending" {
		t.Fatalf("chat press racing an app approval: %d %v", code, body)
	}
	d := cf.planDetail(t, p.ID)
	if len(d.Approvals) != 1 || d.Approvals[0].Via != changeplan.ViaUI ||
		d.Approvals[0].Mode != changeplan.ModeAll || d.Approvals[0].VoidedAt != nil {
		t.Fatalf("approvals %+v, want only the app's approve-all, live", d.Approvals)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a refused decision kept the card used: %+v", card)
	}
}
