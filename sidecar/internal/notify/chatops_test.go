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

	"github.com/pg-sage/sidecar/internal/chatops"
)

// ChatOps approval requests: an approval-needed event for a Sage SRE
// proposal carries Approve/Deny buttons on Slack channels configured for
// interactivity and on Telegram channels. Buttons carry only the
// decision and the proposal id; the callback is verified server side.

const chatProposal = "44444444-4444-4444-8444-444444444444"

func approvalEvent() Event {
	return ActionApprovalEvent(ActionApproval{Database: "orders",
		Title: "Cancel blocking backend pid 5151", Summary: "pid 5151 blocks 2 sessions",
		Risk: "moderate", ProposalID: chatProposal, QueueID: 9})
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

func TestActionApprovalEventCarriesTheProposal(t *testing.T) {
	e := approvalEvent()
	if e.Type != "approval_needed" || e.Severity != "warning" ||
		e.Data["approval_proposal_id"] != chatProposal || e.Data["queue_id"] != 9 ||
		e.Data["database"] != "orders" || e.DedupKey != "sre_proposal:"+chatProposal ||
		!strings.Contains(e.Subject, "Cancel blocking backend pid 5151") {
		t.Fatalf("event = %+v", e)
	}
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

func TestSlackApprovalButtonsOnlyForInteractiveChannels(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	sender := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	ch := Channel{Name: "ops", Type: "slack", Config: map[string]string{
		"webhook_url": srv.URL, "interactive": "true", "signing_secret": "s"}}
	if err := sender.Send(context.Background(), ch, approvalEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	buttons := slackButtons(c.body)
	if len(buttons) != 2 || buttons[0]["action_id"] != chatops.SlackApproveAction ||
		buttons[1]["action_id"] != chatops.SlackDenyAction ||
		buttons[0]["value"] != chatProposal || buttons[1]["value"] != chatProposal {
		t.Fatalf("buttons = %v", buttons)
	}
	ch.Config["interactive"] = "false"
	if err := sender.Send(context.Background(), ch, approvalEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if b := slackButtons(c.body); len(b) != 0 {
		t.Fatalf("non-interactive channel got buttons %v", b)
	}
	ch.Config["interactive"] = "true"
	plain := approvalEvent()
	delete(plain.Data, "approval_proposal_id")
	if err := sender.Send(context.Background(), ch, plain); err != nil {
		t.Fatalf("send: %v", err)
	}
	if b := slackButtons(c.body); len(b) != 0 {
		t.Fatalf("an event without a proposal got buttons %v", b)
	}
}

func telegramSender(base string) *TelegramSender {
	s := NewTelegramSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	s.apiBase = base
	return s
}

func TestTelegramSendsApprovalKeyboard(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	s := telegramSender(srv.URL)
	if s.Type() != "telegram" {
		t.Fatalf("type = %q", s.Type())
	}
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001"}}
	if err := s.Send(context.Background(), ch, approvalEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if c.path != "/bot123456:ABC-secret/sendMessage" || c.body["chat_id"] != "-1001" {
		t.Fatalf("request %s %v", c.path, c.body)
	}
	if _, markup := c.body["parse_mode"]; markup {
		t.Fatal("telegram messages must be plain text (no markup injection)")
	}
	text, _ := c.body["text"].(string)
	if !strings.Contains(text, "pid 5151 blocks 2 sessions") || !strings.Contains(text, "orders") {
		t.Fatalf("text = %q", text)
	}
	raw, _ := json.Marshal(c.body["reply_markup"])
	for _, want := range []string{chatops.CallbackData(chatops.DecisionApprove, chatProposal),
		chatops.CallbackData(chatops.DecisionDeny, chatProposal)} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("keyboard %s lacks %q", raw, want)
		}
	}
	other := FindingCriticalEvent("t", "d", "orders")
	if err := s.Send(context.Background(), ch, other); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, ok := c.body["reply_markup"]; ok {
		t.Fatal("a plain event got an approval keyboard")
	}
}

func TestTelegramErrorsNeverLeakTheBotToken(t *testing.T) {
	c := &capture{code: http.StatusUnauthorized}
	srv := c.server(t)
	ch := Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001"}}
	err := telegramSender(srv.URL).Send(context.Background(), ch, approvalEvent())
	if err == nil || strings.Contains(err.Error(), "ABC-secret") {
		t.Fatalf("send error = %v, want an error without the token", err)
	}
	err = telegramSender("http://127.0.0.1:1").Send(context.Background(), ch, approvalEvent())
	if err == nil || strings.Contains(err.Error(), "ABC-secret") {
		t.Fatalf("connection error = %v, want an error without the token", err)
	}
	for name, cfg := range map[string]map[string]string{
		"no token": {"chat_id": "-1001"}, "no chat": {"bot_token": "1:x"},
	} {
		ch := Channel{Name: "tg", Type: "telegram", Config: cfg}
		if err := telegramSender(srv.URL).Send(context.Background(), ch,
			approvalEvent()); err == nil {
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
	key := make([]byte, 32)
	sealed, err := SealSecrets(map[string]string{"bot_token": "1:secret",
		"chat_id": "-1001"}, key)
	if err != nil || !strings.HasPrefix(sealed["bot_token"], "enc:v1:") ||
		sealed["chat_id"] != "-1001" {
		t.Fatalf("sealed = %v, %v", sealed, err)
	}
}
