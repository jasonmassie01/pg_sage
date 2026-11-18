package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
)

// Plan cards from chat (plan-03b) follow the approval-card rules: a signed
// callback from a chat user mapped to an operator or admin, a token issued
// to this channel, unused and unexpired, naming a plan whose hash has not
// changed. The decision runs plan-03a's coverage rule with via chat, and
// only steps of risk tier safe or moderate are approvable from chat (D7).

var allChoices = []chatops.PlanChoice{chatops.PlanApproveAll, chatops.PlanApproveNext,
	chatops.PlanReject}

func TestChatPlanApproveAllFromSlackCoversEveryStep(t *testing.T) {
	cf := newChatCardFixture(t)
	// Three tables: the default plan caps allow two new indexes per table.
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table+"_b"),
		moderateStep(3, cf.table+"_c"))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		token, testSigningSecret)
	if code != http.StatusOK || body["status"] != "approved" || body["decision"] !=
		"approve_all" || body["plan_id"] != float64(p.ID) || body["database"] != "orders" {
		t.Fatalf("approve all: %d %v", code, body)
	}
	d := cf.planDetail(t, p.ID)
	if d.Plan.Status != changeplan.StatusApproved || len(d.Approvals) != 1 {
		t.Fatalf("plan after approve: %s, approvals %+v", d.Plan.Status, d.Approvals)
	}
	a := d.Approvals[0]
	if a.Via != changeplan.ViaChat || a.Mode != changeplan.ModeAll ||
		a.ApproverUserID != cf.operator || a.PlanHash != p.PlanHash {
		t.Fatalf("approval %+v, want all via chat by %d", a, cf.operator)
	}
	for _, c := range d.Coverage {
		if !c.Covered || c.By != changeplan.CoveredByPlan {
			t.Fatalf("step %d not covered: %+v", c.Ordinal, c)
		}
	}
	card := cf.planCard(t, token)
	if card.UsedAt == nil || card.UsedBy == nil || *card.UsedBy != cf.operator ||
		card.Decision != chatops.PlanApproveAll {
		t.Fatalf("card record %+v", card)
	}
	if reply := cf.replies.last(); !strings.Contains(reply, "Approved") ||
		!strings.Contains(reply, "all 3 steps") {
		t.Fatalf("reply %q", reply)
	}
	// The same card again: consumed exactly once.
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanRejectAction, token,
		testSigningSecret)
	if code != http.StatusConflict || body["code"] != "card_used" {
		t.Fatalf("reused plan card: %d %v", code, body)
	}
	if got := cf.planDetail(t, p.ID); got.Plan.Status != changeplan.StatusApproved {
		t.Fatalf("a used card changed the plan to %s", got.Plan.Status)
	}
}

func TestChatPlanApproveUpToStopsBeforeTheHighTierStep(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table), highStep(3))
	token := cf.issuePlan(t, cf.tgCh, p, 2, chatops.PlanApproveUpTo,
		chatops.PlanApproveNext, chatops.PlanReject)
	code, body := cf.telegramPlanPress(t, chatops.PlanApproveUpTo, token)
	if code != http.StatusOK || body["decision"] != "approve_up_to" ||
		body["up_to"] != float64(2) {
		t.Fatalf("approve up to: %d %v", code, body)
	}
	d := cf.planDetail(t, p.ID)
	a := d.Approvals[0]
	if a.Mode != changeplan.ModeUpTo || a.UpTo == nil || *a.UpTo != 2 ||
		a.Via != changeplan.ViaChat {
		t.Fatalf("approval %+v, want up_to 2 via chat", a)
	}
	if !d.Coverage[0].Covered || !d.Coverage[1].Covered || d.Coverage[2].Covered {
		t.Fatalf("coverage %+v, want steps 1 and 2 only", d.Coverage)
	}
	if reply := cf.replies.last(); !strings.Contains(reply, "step 3") ||
		!strings.Contains(reply, "pg_sage app") {
		t.Fatalf("reply %q does not say the rest needs the app", reply)
	}
	if card := cf.planCard(t, token); card.MessageID != 9003 {
		t.Fatalf("the chat message of the decision was not kept: %+v", card)
	}
}

func TestChatPlanApproveNextAndRejectFromTelegram(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.tgCh, p, 0, allChoices...)
	code, body := cf.telegramPlanPress(t, chatops.PlanApproveNext, token)
	if code != http.StatusOK || body["decision"] != "approve_next" {
		t.Fatalf("approve step 1: %d %v", code, body)
	}
	a := cf.planDetail(t, p.ID).Approvals[0]
	if a.Mode != changeplan.ModeUpTo || a.UpTo == nil || *a.UpTo != 1 {
		t.Fatalf("approve step 1 recorded %+v, want up_to 1", a)
	}
	q := cf.createPlan(t, moderateStep(1, cf.table))
	reject := cf.issuePlan(t, cf.tgCh, q, 0, chatops.PlanApproveAll, chatops.PlanReject)
	code, body = cf.telegramPlanPress(t, chatops.PlanReject, reject)
	if code != http.StatusOK || body["status"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	if got := cf.planDetail(t, q.ID); got.Plan.Status != changeplan.StatusRejected ||
		len(got.Approvals) != 0 {
		t.Fatalf("plan after reject: %s %+v", got.Plan.Status, got.Approvals)
	}
}

func TestChatPlanRefusesBadCards(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	slack := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	expired := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	if _, err := cf.pool.Exec(context.Background(), `UPDATE sage.plan_card_deliveries
		SET expires_at = now() - interval '1 second' WHERE token_sha256 =
		sha256(convert_to($1, 'UTF8'))`, expired); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, action, token string
		want                int
		code                string
	}{
		{"malformed token", chatops.SlackPlanApproveAllAction, "short",
			http.StatusBadRequest, "invalid_request"},
		{"unknown token", chatops.SlackPlanApproveAllAction, "ZZZZZZZZZZZZZZZZZZZZZZ",
			http.StatusNotFound, "unknown_card"},
		{"another channel's card", chatops.SlackPlanApproveAllAction,
			cf.issuePlan(t, cf.tgCh, p, 0, allChoices...), http.StatusForbidden,
			"wrong_channel"},
		{"expired", chatops.SlackPlanApproveAllAction, expired, http.StatusGone,
			"card_expired"},
	}
	for _, c := range cases {
		code, body := cf.slackPress(t, "U-OP"+cf.suffix, c.action, c.token,
			testSigningSecret)
		if code != c.want || body["code"] != c.code {
			t.Errorf("%s: %d %v, want %d %s", c.name, code, body, c.want, c.code)
		}
	}
	if code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		slack, "not-the-secret"); code != http.StatusUnauthorized ||
		body["code"] != "bad_signature" {
		t.Fatalf("bad signature: %d %v", code, body)
	}
	if got := cf.planDetail(t, p.ID); got.Plan.Status != changeplan.StatusProposed {
		t.Fatalf("a refused card changed the plan to %s", got.Plan.Status)
	}
	if card := cf.planCard(t, slack); card.UsedAt != nil {
		t.Fatalf("a refused press used the card: %+v", card)
	}
}

func TestChatPlanRefusesADecisionTheCardOrTheTiersDoNotOffer(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, highStep(1), moderateStep(2, cf.table))
	// The card offers reject only: approve all is not offered.
	rejectOnly := cf.issuePlan(t, cf.slackCh, p, 0, chatops.PlanReject)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		rejectOnly, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_offered" {
		t.Fatalf("approve on a reject-only card: %d %v", code, body)
	}
	// A card claiming approve all on a plan whose step 1 is high tier: the
	// tiers are checked again at decision time (fail closed).
	forged := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	code, body = cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		forged, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_offered" {
		t.Fatalf("approve all past a high-tier step: %d %v", code, body)
	}
	if reply := cf.replies.last(); !strings.Contains(reply, "pg_sage app") {
		t.Fatalf("reply %q does not send the person to the app", reply)
	}
	for _, token := range []string{rejectOnly, forged} {
		if card := cf.planCard(t, token); card.UsedAt != nil {
			t.Fatalf("a refused approval used the card: %+v", card)
		}
	}
	if got := cf.planDetail(t, p.ID); len(got.Approvals) != 0 {
		t.Fatalf("a refused approval was recorded: %+v", got.Approvals)
	}
}

func TestChatPlanChangedAfterTheCardIsRefused(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	step := p.Steps[1]
	step.DeadlineMinutes = 90
	if _, err := changeplan.NewStore(cf.pool).UpdateStep(context.Background(), p.ID, step,
		planResolver{}); err != nil {
		t.Fatalf("edit the plan: %v", err)
	}
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		token, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "content_changed" {
		t.Fatalf("stale plan card: %d %v", code, body)
	}
	if reply := cf.replies.last(); !strings.HasPrefix(reply, "Refused: this plan changed") {
		t.Fatalf("reply %q", reply)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a stale card was used: %+v", card)
	}
}

func TestChatPlanStoreFailureConsumesNoToken(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, chatops.PlanApproveAll, chatops.PlanReject)
	failPlanDecisions(t, errors.New("connection refused"))
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		token, testSigningSecret)
	if code != http.StatusInternalServerError || strings.Contains(body["error"].(string),
		"connection refused") {
		t.Fatalf("store failure: %d %v", code, body)
	}
	if reply := cf.replies.last(); !strings.Contains(reply,
		"pg_sage could not record the decision") {
		t.Fatalf("reply %q", reply)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil || card.Decision != "" {
		t.Fatalf("a failed decision consumed the card: %+v", card)
	}
}

func TestChatPlanRefusalFromTheStoreKeepsTheCard(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, chatops.PlanApproveAll, chatops.PlanReject)
	failPlanDecisions(t, changeplan.ErrNotApprovable)
	code, body := cf.slackPress(t, "U-OP"+cf.suffix, chatops.SlackPlanApproveAllAction,
		token, testSigningSecret)
	if code != http.StatusConflict || body["code"] != "not_pending" {
		t.Fatalf("refused decision: %d %v", code, body)
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a refused decision consumed the card: %+v", card)
	}
}

func TestChatPlanNeedsAMappedOperator(t *testing.T) {
	cf := newChatCardFixture(t)
	p := cf.createPlan(t, moderateStep(1, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, chatops.PlanApproveAll, chatops.PlanReject)
	for user, want := range map[string]string{"U-VW" + cf.suffix: "forbidden",
		"U-NOBODY" + cf.suffix: "unmapped_user"} {
		code, body := cf.slackPress(t, user, chatops.SlackPlanApproveAllAction, token,
			testSigningSecret)
		if code != http.StatusForbidden || body["code"] != want {
			t.Errorf("%s: %d %v, want 403 %s", user, code, body, want)
		}
	}
	if card := cf.planCard(t, token); card.UsedAt != nil {
		t.Fatalf("a refused user used the card: %+v", card)
	}
}

func TestChatPlanTwoClicksMakeOneDecision(t *testing.T) {
	cf := newChatCardFixture(t)
	second := chatUser(t, cf.pool, auth.RoleOperator)
	if _, err := chatops.NewStore(cf.pool).Link(context.Background(), chatops.Identity{
		Provider: chatops.ProviderSlack, TeamID: "T0001", ExternalUserID: "U-OP2" + cf.suffix,
		UserID: second}); err != nil {
		t.Fatal(err)
	}
	p := cf.createPlan(t, moderateStep(1, cf.table), moderateStep(2, cf.table))
	token := cf.issuePlan(t, cf.slackCh, p, 0, allChoices...)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, user := range []string{"U-OP" + cf.suffix, "U-OP2" + cf.suffix} {
		wg.Add(1)
		go func(user string) {
			defer wg.Done()
			code, _ := cf.slackPress(t, user, chatops.SlackPlanApproveAllAction, token,
				testSigningSecret)
			codes <- code
		}(user)
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusOK] != 1 || got[http.StatusConflict] != 1 {
		t.Fatalf("two clicks answered %v, want one 200 and one 409", got)
	}
	if d := cf.planDetail(t, p.ID); len(d.Approvals) != 1 {
		t.Fatalf("two clicks made %d approvals", len(d.Approvals))
	}
}
