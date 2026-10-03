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

// Approval cards: an approval_needed event for a queued action carries
// the card (who, what, why) and, per interactive Slack or Telegram
// channel, a freshly minted single-use card token on its buttons.

const testCardToken = "AbCdEfGhIjKlMnOpQrStUv"

func cardRef() CardRef {
	return CardRef{Database: "orders", QueueID: 41, CardHash: "abc123",
		Title: "Index recommendation for public.orders", Summary: "38% faster",
		ExpiresAt: time.Now().Add(time.Hour)}
}

func cardEvent() Event {
	evt := QueuedApprovalEvent("Index recommendation for public.orders",
		"CREATE INDEX CONCURRENTLY i ON public.orders (customer_id)", "orders",
		"moderate", 41)
	return WithApprovalCard(evt, cardRef(), "Why it needs you:\n- what-if unverified")
}

func TestQueuedApprovalEventCarriesTheQueueItem(t *testing.T) {
	evt := QueuedApprovalEvent("t", "ANALYZE public.orders", "orders", "safe", 7)
	if evt.Type != "approval_needed" || evt.Data["queue_id"] != 7 ||
		evt.Data["database"] != "orders" || evt.Data["sql"] != "ANALYZE public.orders" ||
		evt.DedupKey != "approval:orders:7" {
		t.Fatalf("event = %+v", evt)
	}
	if _, ok := ApprovalCardOf(evt); ok {
		t.Fatal("a plain queued event must not claim a card")
	}
}

func TestWithApprovalCardKeepsTheEventAndAddsTheCard(t *testing.T) {
	evt := cardEvent()
	ref, ok := ApprovalCardOf(evt)
	if !ok || ref.QueueID != 41 || ref.CardHash != "abc123" || ref.Database != "orders" {
		t.Fatalf("card = %+v, %v", ref, ok)
	}
	if !strings.Contains(evt.Body, "what-if unverified") || evt.Type != "approval_needed" ||
		!strings.Contains(evt.Subject, "Index recommendation") {
		t.Fatalf("event = %+v", evt)
	}
	// The original event's map is not mutated.
	base := QueuedApprovalEvent("t", "s", "d", "r", 1)
	_ = WithApprovalCard(base, cardRef(), "body")
	if _, ok := base.Data[dataApprovalCard]; ok {
		t.Fatal("WithApprovalCard mutated its input")
	}
	if _, ok := ApprovalCardOf(Event{}); ok {
		t.Fatal("empty event has a card")
	}
}

func TestCardInteractive(t *testing.T) {
	cases := []struct {
		ch   Channel
		want bool
	}{
		{Channel{Type: "slack", Config: map[string]string{"interactive": "true",
			"signing_secret": "s"}}, true},
		{Channel{Type: "slack", Config: map[string]string{"interactive": "true"}}, false},
		{Channel{Type: "slack", Config: map[string]string{"signing_secret": "s"}}, false},
		{Channel{Type: "telegram", Config: map[string]string{"webhook_secret": "w"}}, true},
		{Channel{Type: "telegram", Config: map[string]string{}}, false},
		{Channel{Type: "email", Config: map[string]string{"interactive": "true"}}, false},
		{Channel{Type: "slack"}, false},
	}
	for i, c := range cases {
		if got := CardInteractive(c.ch); got != c.want {
			t.Errorf("case %d (%s %v): %v, want %v", i, c.ch.Type, c.ch.Config, got, c.want)
		}
	}
}

type fakeIssuer struct {
	mu      sync.Mutex
	issued  []CardRef
	chans   []int
	revoked []string
	err     error
}

func (f *fakeIssuer) Issue(_ context.Context, ch Channel, ref CardRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.issued = append(f.issued, ref)
	f.chans = append(f.chans, ch.ID)
	return testCardToken, nil
}

func (f *fakeIssuer) Revoke(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
}

type cardSender struct {
	typ    string
	mu     sync.Mutex
	events []Event
	err    error
}

func (s *cardSender) Type() string { return s.typ }

func (s *cardSender) Send(_ context.Context, _ Channel, evt Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
	return s.err
}

// memStore is a rule store over several channels.
type memStore struct {
	rules    []Rule
	channels map[int]*Channel
}

func (m *memStore) MatchingRules(_ context.Context, eventType string) ([]Rule, error) {
	var out []Rule
	for _, r := range m.rules {
		if r.Event == eventType {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) Channel(_ context.Context, id int) (*Channel, error) {
	ch, ok := m.channels[id]
	if !ok {
		return nil, errors.New("channel not found")
	}
	cp := *ch
	return &cp, nil
}

func (m *memStore) LogDelivery(context.Context, int, Event, string, string) error {
	return nil
}

func cardDispatcher(channels map[int]*Channel, issuer CardTokenIssuer,
	senders ...Sender) *Dispatcher {
	rules := make([]Rule, 0, len(channels))
	for id := range channels {
		rules = append(rules, Rule{ID: id, ChannelID: id, Event: "approval_needed",
			MinSeverity: "info", Enabled: true})
	}
	d := NewDispatcherWithStore(&memStore{rules: rules, channels: channels},
		func(string, string, ...any) {})
	for _, s := range senders {
		d.RegisterSender(s)
	}
	if issuer != nil {
		d.WithCardTokens(issuer)
	}
	return d
}

func TestDispatcherMintsATokenPerInteractiveChannel(t *testing.T) {
	slack := &cardSender{typ: "slack"}
	email := &cardSender{typ: "email"}
	issuer := &fakeIssuer{}
	d := cardDispatcher(map[int]*Channel{
		1: {ID: 1, Type: "slack", Enabled: true, Config: map[string]string{
			"interactive": "true", "signing_secret": "s"}},
		2: {ID: 2, Type: "email", Enabled: true, Config: map[string]string{}},
	}, issuer, slack, email)
	if err := d.Dispatch(context.Background(), cardEvent()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(issuer.issued) != 1 || issuer.chans[0] != 1 || issuer.issued[0].QueueID != 41 {
		t.Fatalf("issued %+v for channels %v", issuer.issued, issuer.chans)
	}
	if len(slack.events) != 1 || slack.events[0].Data[DataCardToken] != testCardToken {
		t.Fatalf("slack events = %+v", slack.events)
	}
	if len(email.events) != 1 {
		t.Fatalf("email events = %+v", email.events)
	}
	if _, ok := email.events[0].Data[DataCardToken]; ok {
		t.Fatal("a non-interactive channel received a card token")
	}
}

func TestDispatcherCardTokenEdgeCases(t *testing.T) {
	interactive := func() map[int]*Channel {
		return map[int]*Channel{1: {ID: 1, Type: "slack", Enabled: true,
			Config: map[string]string{"interactive": "true", "signing_secret": "s"}}}
	}
	t.Run("issuer error sends without buttons", func(t *testing.T) {
		slack := &cardSender{typ: "slack"}
		d := cardDispatcher(interactive(), &fakeIssuer{err: errors.New("db down")}, slack)
		if err := d.Dispatch(context.Background(), cardEvent()); err != nil {
			t.Fatal(err)
		}
		if len(slack.events) != 1 || slack.events[0].Data[DataCardToken] != nil {
			t.Fatalf("events = %+v", slack.events)
		}
	})
	t.Run("failed send revokes the token", func(t *testing.T) {
		slack := &cardSender{typ: "slack", err: errors.New("503")}
		issuer := &fakeIssuer{}
		d := cardDispatcher(interactive(), issuer, slack)
		_ = d.Dispatch(context.Background(), cardEvent())
		if len(issuer.revoked) != 1 || issuer.revoked[0] != testCardToken {
			t.Fatalf("revoked = %v", issuer.revoked)
		}
	})
	t.Run("no issuer means no token", func(t *testing.T) {
		slack := &cardSender{typ: "slack"}
		d := cardDispatcher(interactive(), nil, slack)
		_ = d.Dispatch(context.Background(), cardEvent())
		if len(slack.events) != 1 || slack.events[0].Data[DataCardToken] != nil {
			t.Fatalf("events = %+v", slack.events)
		}
	})
	t.Run("events without a card get no token", func(t *testing.T) {
		slack := &cardSender{typ: "slack"}
		issuer := &fakeIssuer{}
		d := cardDispatcher(interactive(), issuer, slack)
		_ = d.Dispatch(context.Background(), QueuedApprovalEvent("t", "s", "d", "r", 1))
		if len(issuer.issued) != 0 || slack.events[0].Data[DataCardToken] != nil {
			t.Fatalf("issued %v", issuer.issued)
		}
	})
	t.Run("sre proposal events keep their own buttons", func(t *testing.T) {
		slack := &cardSender{typ: "slack"}
		issuer := &fakeIssuer{}
		d := cardDispatcher(interactive(), issuer, slack)
		evt := WithApprovalCard(approvalEvent(), cardRef(), "body")
		_ = d.Dispatch(context.Background(), evt)
		if len(issuer.issued) != 0 {
			t.Fatalf("an SRE proposal got a card token: %v", issuer.issued)
		}
	})
}

func TestSlackRendersCardButtons(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	evt := cardEvent()
	evt.Data[DataCardToken] = testCardToken
	if err := sender.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	buttons := slackButtons(c.body)
	if len(buttons) != 3 {
		t.Fatalf("buttons = %v", buttons)
	}
	want := []string{chatops.SlackCardApproveAction, chatops.SlackCardRejectAction,
		chatops.SlackCardSnoozeAction}
	for i, b := range buttons {
		if b["action_id"] != want[i] || b["value"] != testCardToken {
			t.Fatalf("button %d = %v", i, b)
		}
	}
	if buttons[0]["confirm"] == nil {
		t.Fatal("approve must ask for confirmation")
	}
	raw, _ := json.Marshal(c.body)
	if !strings.Contains(string(raw), "what-if unverified") {
		t.Fatalf("payload lacks the why: %s", raw)
	}
	// Without a token, an interactive channel shows the card but no buttons.
	if err := sender.Send(context.Background(), ch, cardEvent()); err != nil {
		t.Fatal(err)
	}
	if b := slackButtons(c.body); len(b) != 0 {
		t.Fatalf("buttons without a token: %v", b)
	}
}

func TestTelegramRendersCardKeyboardAndReplies(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	s := telegramSender(srv.URL)
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001", "webhook_secret": "w"}}
	evt := cardEvent()
	evt.Data[DataCardToken] = testCardToken
	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	raw, _ := json.Marshal(c.body["reply_markup"])
	for _, d := range []chatops.Decision{chatops.DecisionApprove, chatops.DecisionDeny,
		chatops.DecisionSnooze} {
		if !strings.Contains(string(raw), chatops.CardCallbackData(d, testCardToken)) {
			t.Fatalf("keyboard %s lacks %s", raw, d)
		}
	}
	text, _ := c.body["text"].(string)
	if !strings.Contains(text, "what-if unverified") {
		t.Fatalf("text = %q", text)
	}
	follow := ApprovalOutcomeEvent(ApprovalOutcome{Database: "orders", QueueID: 41,
		Title: "Index recommendation", Verdict: "verified", Detail: "p95 12.1ms -> 3.0ms",
		ReplyTo: 9001})
	if err := s.Send(context.Background(), ch, follow); err != nil {
		t.Fatalf("send follow-up: %v", err)
	}
	if c.body["reply_to_message_id"] != float64(9001) {
		t.Fatalf("follow-up is not a reply: %v", c.body)
	}
	if _, ok := c.body["reply_markup"]; ok {
		t.Fatal("a follow-up must not carry buttons")
	}
}

func TestApprovalOutcomeEvent(t *testing.T) {
	cases := map[string]string{"verified": "Verified", "rolled_back": "Rolled back",
		"regressed": "Regressed", "failed": "Failed", "rejected": "Rejected",
		"expired": "Expired", "something_else": "Closed"}
	for verdict, prefix := range cases {
		e := ApprovalOutcomeEvent(ApprovalOutcome{Database: "orders", QueueID: 3,
			Title: "Drop index", Verdict: verdict, Detail: "d", Predicted: "38% faster"})
		if e.Type != "approval_outcome" || !strings.HasPrefix(e.Subject, prefix) ||
			!strings.Contains(e.Subject, "Drop index") || e.Data["verdict"] != verdict ||
			!strings.Contains(e.Body, "d") || !strings.Contains(e.Body, "38% faster") ||
			e.Data["queue_id"] != 3 {
			t.Errorf("%s: %+v", verdict, e)
		}
		if _, ok := e.Data[DataReplyTo]; ok {
			t.Errorf("%s: reply_to set without a message", verdict)
		}
	}
	if e := ApprovalOutcomeEvent(ApprovalOutcome{Verdict: "regressed"}); e.Severity != "warning" {
		t.Fatalf("regression severity = %q", e.Severity)
	}
}

func TestSendToChannel(t *testing.T) {
	slack := &cardSender{typ: "slack"}
	d := cardDispatcher(map[int]*Channel{
		1: {ID: 1, Type: "slack", Enabled: true, Config: map[string]string{}},
		2: {ID: 2, Type: "slack", Enabled: false, Config: map[string]string{}},
	}, nil, slack)
	evt := ApprovalOutcomeEvent(ApprovalOutcome{Verdict: "verified", Title: "x"})
	if err := d.SendToChannel(context.Background(), 1, evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(slack.events) != 1 || slack.events[0].Type != "approval_outcome" {
		t.Fatalf("events = %+v", slack.events)
	}
	if err := d.SendToChannel(context.Background(), 2, evt); !errors.Is(err,
		ErrChannelDisabled) {
		t.Fatalf("disabled channel: err = %v", err)
	}
	if err := d.SendToChannel(context.Background(), 99, evt); err == nil {
		t.Fatal("unknown channel sent")
	}
	if len(slack.events) != 1 {
		t.Fatalf("events after refusals = %d", len(slack.events))
	}
}
