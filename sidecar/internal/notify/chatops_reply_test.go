package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// Replies to a chat decision: Telegram acknowledges the button press and
// posts the outcome in the channel's chat; Slack answers on the
// interaction's response_url, which must be Slack's own hook host.

type callLog struct {
	mu    sync.Mutex
	paths []string
	body  []map[string]any
}

func (l *callLog) server(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		l.mu.Lock()
		l.paths = append(l.paths, r.URL.Path)
		l.body = append(l.body, body)
		l.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tgChannel() Channel {
	return Channel{Name: "tg", Type: "telegram", Config: map[string]string{
		"bot_token": "123456:ABC-secret", "chat_id": "-1001"}}
}

func TestTelegramReplyAcknowledgesAndPostsTheOutcome(t *testing.T) {
	l := &callLog{}
	srv := l.server(t, http.StatusOK)
	a := chatops.Action{Provider: chatops.ProviderTelegram, CallbackID: "cbq-9",
		ChatID: "-1001"}
	if err := telegramSender(srv.URL).Reply(context.Background(), tgChannel(), a,
		"Approved by alice: <b>executed</b>"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(l.paths) != 2 || l.paths[0] != "/bot123456:ABC-secret/answerCallbackQuery" ||
		l.paths[1] != "/bot123456:ABC-secret/sendMessage" {
		t.Fatalf("calls = %v, want answerCallbackQuery then sendMessage", l.paths)
	}
	if l.body[0]["callback_query_id"] != "cbq-9" {
		t.Fatalf("answer = %v", l.body[0])
	}
	if l.body[1]["chat_id"] != "-1001" ||
		l.body[1]["text"] != "Approved by alice: <b>executed</b>" {
		t.Fatalf("message = %v", l.body[1])
	}
	if _, markup := l.body[1]["parse_mode"]; markup {
		t.Fatal("replies must be plain text")
	}
}

func TestTelegramReplyWithoutCallbackOnlyPosts(t *testing.T) {
	l := &callLog{}
	srv := l.server(t, http.StatusOK)
	if err := telegramSender(srv.URL).Reply(context.Background(), tgChannel(),
		chatops.Action{Provider: chatops.ProviderTelegram}, "done"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if len(l.paths) != 1 || !strings.HasSuffix(l.paths[0], "/sendMessage") {
		t.Fatalf("calls = %v, want only sendMessage", l.paths)
	}
}

func TestTelegramReplyErrorsNeverLeakTheToken(t *testing.T) {
	l := &callLog{}
	srv := l.server(t, http.StatusBadRequest)
	err := telegramSender(srv.URL).Reply(context.Background(), tgChannel(),
		chatops.Action{CallbackID: "x"}, "done")
	if err == nil || strings.Contains(err.Error(), "ABC-secret") {
		t.Fatalf("reply error = %v, want an error without the token", err)
	}
	if len(l.paths) != 1 {
		t.Fatalf("calls after a failed acknowledgement = %v, want 1", l.paths)
	}
}

func TestSlackReplyPostsToTheResponseURL(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	s := NewSlackSenderWithPolicy(TargetPolicy{AllowPrivate: true})
	a := chatops.Action{Provider: chatops.ProviderSlack, ResponseURL: srv.URL + "/actions/1"}
	if err := s.Reply(context.Background(), Channel{Name: "ops"}, a, "Denied by alice"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if c.path != "/actions/1" || c.body["text"] != "Denied by alice" ||
		c.body["replace_original"] != false {
		t.Fatalf("reply request %s %v", c.path, c.body)
	}
}

func TestSlackReplyRefusesForeignResponseURLs(t *testing.T) {
	s := NewSlackSender()
	for name, u := range map[string]string{
		"empty":         "",
		"other host":    "https://evil.example/actions/1",
		"plain http":    "http://hooks.slack.com/actions/1",
		"metadata host": "https://169.254.169.254/latest",
		"lookalike":     "https://hooks.slack.com.evil.example/actions/1",
	} {
		a := chatops.Action{Provider: chatops.ProviderSlack, ResponseURL: u}
		err := s.Reply(context.Background(), Channel{Name: "ops"}, a, "x")
		if !errors.Is(err, ErrTargetBlocked) {
			t.Errorf("%s: reply = %v, want ErrTargetBlocked", name, err)
		}
	}
}
