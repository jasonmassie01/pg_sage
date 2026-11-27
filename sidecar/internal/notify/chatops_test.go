package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// ChatOps approval requests: a Sage SRE backend cancel is an approval
// card like every other queued action (policy-08). Its buttons carry only
// a card token; a backend-signal card has no Snooze (its evidence is
// minutes old) and lives at most 15 minutes. The retired proposal buttons
// are never rendered again.

// sreCardEvent is the card event of a queued Sage SRE cancel.
func sreCardEvent() Event {
	evt := QueuedApprovalEvent("Cancel blocking backend pid 5151",
		"SELECT pg_cancel_backend(5151)", "orders", "moderate", 9)
	evt.Data["approval_proposal_id"] = "44444444-4444-4444-8444-444444444444"
	return WithApprovalCard(evt, CardRef{Database: "orders", QueueID: 9, CardHash: "h9",
		Title: "Cancel blocking backend pid 5151", ExpiresAt: time.Now().Add(time.Hour),
		MaxTTL: 15 * time.Minute, NoSnooze: true}, "orders: pid 5151 blocks 2 sessions")
}

type capture struct {
	mu   sync.Mutex
	path string
	body map[string]any
	code int
}

func (c *capture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.path = r.URL.Path
		c.body = nil
		_ = json.Unmarshal(raw, &c.body)
		code := c.code
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func slackButtons(body map[string]any) []map[string]any {
	var out []map[string]any
	blocks, _ := body["blocks"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["type"] != "actions" {
			continue
		}
		elems, _ := block["elements"].([]any)
		for _, e := range elems {
			m, _ := e.(map[string]any)
			out = append(out, m)
		}
	}
	return out
}

func TestSlackNeverRendersRetiredProposalButtons(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	// No card token (the issuer failed): the card goes out without any
	// buttons, even though the event still names a proposal.
	if err := sender.Send(context.Background(), ch, sreCardEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if b := slackButtons(c.body); len(b) != 0 {
		t.Fatalf("an SRE event without a card token got buttons %v", b)
	}
	raw, _ := json.Marshal(c.body)
	if strings.Contains(string(raw), "sage_approve") || strings.Contains(string(raw),
		"sage_deny") {
		t.Fatalf("payload carries a retired proposal button: %s", raw)
	}
}

func TestSlackBackendSignalCardHasNoSnooze(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	evt := sreCardEvent()
	evt.Data[DataCardToken] = testCardToken
	if err := sender.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	buttons := slackButtons(c.body)
	if len(buttons) != 2 || buttons[0]["action_id"] != chatops.SlackCardApproveAction ||
		buttons[1]["action_id"] != chatops.SlackCardRejectAction ||
		buttons[0]["value"] != testCardToken || buttons[1]["value"] != testCardToken ||
		buttons[0]["confirm"] == nil {
		t.Fatalf("backend-signal buttons = %v", buttons)
	}
}

func telegramSender(base string) *TelegramSender {
	s := NewTelegramSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	s.apiBase = base
	return s
}

func TestTelegramSendsBackendSignalCardKeyboard(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	s := telegramSender(srv.URL)
	if s.Type() != "telegram" {
		t.Fatalf("type = %q", s.Type())
	}
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001", "webhook_secret": "w"}}
	evt := sreCardEvent()
	evt.Data[DataCardToken] = testCardToken
	if err := s.Send(context.Background(), ch, evt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if c.path != "/bot123456:ABC-secret/sendMessage" || c.body["chat_id"] != "-1001" {
		t.Fatalf("request %s %v", c.path, c.body)
	}
	if _, markup := c.body["parse_mode"]; markup {
		t.Fatal("telegram messages must be plain text (no markup injection)")
	}
	text, _ := c.body["text"].(string)
	if !strings.Contains(text, "pid 5151 blocks 2 sessions") {
		t.Fatalf("text = %q", text)
	}
	raw, _ := json.Marshal(c.body["reply_markup"])
	for _, want := range []string{
		chatops.CardCallbackData(chatops.DecisionApprove, testCardToken),
		chatops.CardCallbackData(chatops.DecisionDeny, testCardToken)} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("keyboard %s lacks %q", raw, want)
		}
	}
	if strings.Contains(string(raw), chatops.CardCallbackData(chatops.DecisionSnooze,
		testCardToken)) || strings.Contains(string(raw), "sage:a:") {
		t.Fatalf("keyboard %s offers snooze or a retired proposal button", raw)
	}
	// Without a token there is no keyboard at all.
	if err := s.Send(context.Background(), ch, sreCardEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, ok := c.body["reply_markup"]; ok {
		t.Fatal("an SRE event without a card token got a keyboard")
	}
}

func TestTelegramErrorsNeverLeakTheBotToken(t *testing.T) {
	c := &capture{code: http.StatusUnauthorized}
	srv := c.server(t)
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001"}}
	err := telegramSender(srv.URL).Send(context.Background(), ch, sreCardEvent())
	if err == nil || strings.Contains(err.Error(), "ABC-secret") {
		t.Fatalf("send error = %v, want an error without the token", err)
	}
	err = telegramSender("http://127.0.0.1:1").Send(context.Background(), ch, sreCardEvent())
	if err == nil || strings.Contains(err.Error(), "ABC-secret") {
		t.Fatalf("connection error = %v, want an error without the token", err)
	}
	for name, cfg := range map[string]map[string]string{
		"no token": {"chat_id": "-1001"}, "no chat": {"bot_token": "1:x"},
	} {
		ch := Channel{Name: "tg", Type: "telegram", Config: cfg}
		if err := telegramSender(srv.URL).Send(context.Background(), ch,
			sreCardEvent()); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
}

func TestChatOpsSecretsAreSealed(t *testing.T) {
	for _, k := range []string{"signing_secret", "bot_token", "webhook_secret"} {
		found := false
		for _, s := range SecretConfigKeys {
			found = found || s == k
		}
		if !found {
			t.Fatalf("%s is not a secret config key", k)
		}
	}
	key := testKeyring(t, make([]byte, 32))
	sealed, err := SealSecrets(map[string]string{"bot_token": "1:secret",
		"chat_id": "-1001"}, key, 1)
	if err != nil || !strings.HasPrefix(sealed["bot_token"], crypto.SealedPrefix) ||
		sealed["chat_id"] != "-1001" {
		t.Fatalf("sealed = %v, %v", sealed, err)
	}
}
