package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// plan-03b: a proposed plan goes to the channels that take approval cards,
// with the buttons its risk tiers allow from chat (always Reject) carrying
// a single-use plan token.

func planRef(offered []chatops.PlanChoice, upTo int) PlanCardRef {
	return PlanCardRef{Database: "orders", PlanID: 7,
		PlanHash: strings.Repeat("ab", 32), Title: "cut blocks per call", Steps: 3,
		Offered: offered, UpTo: upTo, ExpiresAt: time.Now().Add(time.Hour)}
}

type fakePlanIssuer struct {
	mu      sync.Mutex
	issued  []PlanCardRef
	revoked []string
	err     error
}

func (f *fakePlanIssuer) Issue(_ context.Context, _ Channel, ref PlanCardRef) (string,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.issued = append(f.issued, ref)
	return testCardToken, nil
}

func (f *fakePlanIssuer) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
}

var allOffer = []chatops.PlanChoice{chatops.PlanApproveAll, chatops.PlanApproveNext,
	chatops.PlanReject}

func TestPlanProposedEvent(t *testing.T) {
	evt := PlanProposedEvent(planRef(allOffer, 0), "Goal: cut blocks per call")
	ref, ok := PlanCardOf(evt)
	if !ok || ref.PlanID != 7 || evt.Type != "approval_needed" || evt.Severity != "warning" ||
		evt.DedupKey != "plan:orders:7:"+strings.Repeat("ab", 32) ||
		!strings.Contains(evt.Subject, "cut blocks per call") ||
		!strings.Contains(evt.Body, "Goal:") || evt.Data["plan_id"] != int64(7) {
		t.Fatalf("event %+v", evt)
	}
	if _, ok := ApprovalCardOf(evt); ok {
		t.Fatal("a plan card is not an approval card")
	}
	if _, ok := FactCardOf(evt); ok {
		t.Fatal("a plan card is not a fact card")
	}
	if _, ok := PlanCardOf(cardEvent()); ok {
		t.Fatal("an approval card is not a plan card")
	}
	if queueable(evt) {
		t.Fatal("a plan card carries a typed reference: it must be delivered at once")
	}
}

func TestDispatcherMintsPlanTokens(t *testing.T) {
	interactive := map[int]*Channel{1: {ID: 1, Type: "slack", Enabled: true,
		Config: map[string]string{"interactive": "true", "signing_secret": "s"}},
		2: {ID: 2, Type: "email", Enabled: true, Config: map[string]string{}}}
	slack, email := &cardSender{typ: "slack"}, &cardSender{typ: "email"}
	issuer := &fakePlanIssuer{}
	d := cardDispatcher(interactive, nil, slack, email).WithPlanTokens(issuer)
	if err := d.Dispatch(context.Background(), PlanProposedEvent(planRef(allOffer, 0),
		"b")); err != nil {
		t.Fatal(err)
	}
	if len(issuer.issued) != 1 || len(slack.events) != 1 ||
		slack.events[0].Data[DataPlanToken] != testCardToken ||
		email.events[0].Data[DataPlanToken] != nil {
		t.Fatalf("issued %+v slack %+v email %+v", issuer.issued, slack.events, email.events)
	}
	failing := &cardSender{typ: "slack", err: errors.New("503")}
	issuer = &fakePlanIssuer{}
	d = cardDispatcher(map[int]*Channel{1: interactive[1]}, nil, failing).
		WithPlanTokens(issuer)
	_ = d.Dispatch(context.Background(), PlanProposedEvent(planRef(allOffer, 0), "b"))
	if len(issuer.revoked) != 1 {
		t.Fatalf("an undelivered plan card keeps its token: %v", issuer.revoked)
	}
	// An issuer failure sends the card without buttons (decide in the app).
	issuer = &fakePlanIssuer{err: errors.New("control db down")}
	plain := &cardSender{typ: "slack"}
	d = cardDispatcher(map[int]*Channel{1: interactive[1]}, nil, plain).WithPlanTokens(issuer)
	_ = d.Dispatch(context.Background(), PlanProposedEvent(planRef(allOffer, 0), "b"))
	if len(plain.events) != 1 || plain.events[0].Data[DataPlanToken] != nil {
		t.Fatalf("issuer failure: %+v", plain.events)
	}
	// An approval card never gets a plan token.
	issuer = &fakePlanIssuer{}
	plain = &cardSender{typ: "slack"}
	d = cardDispatcher(map[int]*Channel{1: interactive[1]}, nil, plain).WithPlanTokens(issuer)
	_ = d.Dispatch(context.Background(), cardEvent())
	if len(issuer.issued) != 0 || plain.events[0].Data[DataPlanToken] != nil {
		t.Fatalf("approval card got a plan token: %+v", issuer.issued)
	}
}

func slackPlanCard(t *testing.T, ref PlanCardRef) []map[string]any {
	t.Helper()
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	evt := PlanProposedEvent(ref, "Goal: cut blocks per call")
	evt.Data[DataPlanToken] = testCardToken
	if err := sender.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}
	return slackButtons(c.body)
}

func buttonTexts(buttons []map[string]any) []string {
	var out []string
	for _, b := range buttons {
		text, _ := b["text"].(map[string]any)
		out = append(out, b["action_id"].(string)+"="+text["text"].(string))
	}
	return out
}

func TestSlackRendersOnlyTheOfferedPlanButtons(t *testing.T) {
	cases := map[string]struct {
		ref  PlanCardRef
		want []string
	}{
		"all": {planRef(allOffer, 0), []string{
			chatops.SlackPlanApproveAllAction + "=Approve all",
			chatops.SlackPlanApproveNextAction + "=Approve step 1 only",
			chatops.SlackPlanRejectAction + "=Reject"}},
		"up to 2": {planRef([]chatops.PlanChoice{chatops.PlanApproveUpTo,
			chatops.PlanApproveNext, chatops.PlanReject}, 2), []string{
			chatops.SlackPlanApproveUpToAction + "=Approve up to step 2",
			chatops.SlackPlanApproveNextAction + "=Approve step 1 only",
			chatops.SlackPlanRejectAction + "=Reject"}},
		"reject only": {planRef([]chatops.PlanChoice{chatops.PlanReject}, 0), []string{
			chatops.SlackPlanRejectAction + "=Reject"}},
	}
	for name, c := range cases {
		buttons := slackPlanCard(t, c.ref)
		got := buttonTexts(buttons)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: buttons %v, want %v", name, got, c.want)
		}
		for _, b := range buttons {
			if b["value"] != testCardToken {
				t.Errorf("%s: button %v does not carry the token", name, b)
			}
		}
	}
}

func TestTelegramRendersOnlyTheOfferedPlanButtons(t *testing.T) {
	tc := &capture{}
	tsrv := tc.server(t)
	tg := telegramSender(tsrv.URL)
	tch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001", "webhook_secret": "w"}}
	ref := planRef([]chatops.PlanChoice{chatops.PlanApproveUpTo, chatops.PlanApproveNext,
		chatops.PlanReject}, 2)
	evt := PlanProposedEvent(ref, "Goal")
	evt.Data[DataPlanToken] = testCardToken
	if err := tg.Send(context.Background(), tch, evt); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tc.body["reply_markup"])
	for _, c := range ref.Offered {
		if !strings.Contains(string(raw), chatops.PlanCallbackData(c, testCardToken)) {
			t.Fatalf("keyboard %s lacks %s", raw, c)
		}
	}
	if strings.Contains(string(raw), chatops.PlanCallbackData(chatops.PlanApproveAll,
		testCardToken)) || !strings.Contains(string(raw), "Approve up to step 2") {
		t.Fatalf("keyboard %s offers approve all or lacks the up-to label", raw)
	}
}

func TestPlanCardWithoutTokenHasNoButtons(t *testing.T) {
	msg := telegramMessage(PlanProposedEvent(planRef(allOffer, 0), "Goal"))
	if _, ok := msg["reply_markup"]; ok {
		t.Fatalf("a plan card without a token got buttons: %v", msg)
	}
}
