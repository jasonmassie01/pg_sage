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

// Fact cards (roadmap 2.3): a proposed fact goes to the channels that take
// approvals, with Confirm / Reject buttons carrying a single-use token.

func factRef() FactCardRef {
	return FactCardRef{Database: "orders", FactID: 12, FactHash: "fh1",
		Title:     "index public.idx_thesis_run is owned by the application's migrations",
		ExpiresAt: time.Now().Add(time.Hour)}
}

type fakeFactIssuer struct {
	mu      sync.Mutex
	issued  []FactCardRef
	revoked []string
	err     error
}

func (f *fakeFactIssuer) Issue(_ context.Context, _ Channel, ref FactCardRef) (string,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.issued = append(f.issued, ref)
	return testCardToken, nil
}

func (f *fakeFactIssuer) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
}

func TestFactProposedEvent(t *testing.T) {
	evt := FactProposedEvent(factRef(), "Evidence:\n- dropped 8 times, came back")
	ref, ok := FactCardOf(evt)
	if !ok || ref.FactID != 12 || evt.Type != "approval_needed" ||
		evt.DedupKey != "fact:orders:12" || !strings.Contains(evt.Subject, "Confirm") ||
		!strings.Contains(evt.Body, "dropped 8 times") {
		t.Fatalf("event %+v", evt)
	}
	if _, ok := ApprovalCardOf(evt); ok {
		t.Fatal("a fact card is not an approval card")
	}
	if _, ok := FactCardOf(cardEvent()); ok {
		t.Fatal("an approval card is not a fact card")
	}
}

func TestDispatcherMintsFactTokens(t *testing.T) {
	interactive := map[int]*Channel{1: {ID: 1, Type: "slack", Enabled: true,
		Config: map[string]string{"interactive": "true", "signing_secret": "s"}},
		2: {ID: 2, Type: "email", Enabled: true, Config: map[string]string{}}}
	slack, email := &cardSender{typ: "slack"}, &cardSender{typ: "email"}
	issuer := &fakeFactIssuer{}
	d := cardDispatcher(interactive, nil, slack, email).WithFactTokens(issuer)
	if err := d.Dispatch(context.Background(), FactProposedEvent(factRef(), "b")); err != nil {
		t.Fatal(err)
	}
	if len(issuer.issued) != 1 || len(slack.events) != 1 ||
		slack.events[0].Data[DataFactToken] != testCardToken ||
		email.events[0].Data[DataFactToken] != nil {
		t.Fatalf("issued %+v slack %+v email %+v", issuer.issued, slack.events, email.events)
	}
	failing := &cardSender{typ: "slack", err: errors.New("503")}
	issuer = &fakeFactIssuer{}
	d = cardDispatcher(map[int]*Channel{1: interactive[1]}, nil, failing).
		WithFactTokens(issuer)
	_ = d.Dispatch(context.Background(), FactProposedEvent(factRef(), "b"))
	if len(issuer.revoked) != 1 {
		t.Fatalf("an undelivered fact card keeps its token: %v", issuer.revoked)
	}
	// An approval card never gets a fact token, and vice versa.
	issuer = &fakeFactIssuer{}
	plain := &cardSender{typ: "slack"}
	d = cardDispatcher(map[int]*Channel{1: interactive[1]}, nil, plain).WithFactTokens(issuer)
	_ = d.Dispatch(context.Background(), cardEvent())
	if len(issuer.issued) != 0 || plain.events[0].Data[DataFactToken] != nil {
		t.Fatalf("approval card got a fact token: %+v", issuer.issued)
	}
}

func TestSlackAndTelegramRenderFactButtons(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	evt := FactProposedEvent(factRef(), "Evidence: recreated 8 times")
	evt.Data[DataFactToken] = testCardToken
	if err := sender.Send(context.Background(), ch, evt); err != nil {
		t.Fatal(err)
	}
	buttons := slackButtons(c.body)
	if len(buttons) != 2 || buttons[0]["action_id"] != chatops.SlackFactConfirmAction ||
		buttons[1]["action_id"] != chatops.SlackFactRejectAction ||
		buttons[0]["value"] != testCardToken {
		t.Fatalf("slack buttons %v", buttons)
	}
	tc := &capture{}
	tsrv := tc.server(t)
	tg := telegramSender(tsrv.URL)
	tch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001", "webhook_secret": "w"}}
	if err := tg.Send(context.Background(), tch, evt); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tc.body["reply_markup"])
	for _, d := range []chatops.Decision{chatops.DecisionApprove, chatops.DecisionDeny} {
		if !strings.Contains(string(raw), chatops.FactCallbackData(d, testCardToken)) {
			t.Fatalf("keyboard %s lacks %s", raw, d)
		}
	}
}
