package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// telegramTextMax is Telegram's message length limit.
const telegramTextMax = 4096

// TelegramSender delivers notifications through the Telegram Bot API.
// Messages are plain text (no parse mode), so event text cannot inject
// markup or links.
type TelegramSender struct {
	client  *http.Client
	policy  TargetPolicy
	apiBase string
}

// NewTelegramSender creates a TelegramSender that refuses internal
// targets.
func NewTelegramSender() *TelegramSender {
	return NewTelegramSenderWithPolicy(TargetPolicy{})
}

// NewTelegramSenderWithPolicy creates a TelegramSender with an explicit
// target policy.
func NewTelegramSenderWithPolicy(policy TargetPolicy) *TelegramSender {
	return &TelegramSender{client: policy.HTTPClient(10 * time.Second), policy: policy,
		apiBase: "https://api.telegram.org"}
}

// Type returns the channel type identifier.
func (s *TelegramSender) Type() string { return "telegram" }

// Send posts the event to the channel's chat, with Approve/Deny buttons
// when the event asks for a decision on a proposal.
func (s *TelegramSender) Send(ctx context.Context, ch Channel, evt Event) error {
	text := evt.Subject
	if evt.Body != "" {
		text += "\n\n" + evt.Body
	}
	msg := map[string]any{"text": TruncateRunes(text, telegramTextMax),
		"disable_web_page_preview": true}
	if id, ok := approvalProposal(evt); ok {
		msg["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "Approve", "callback_data": chatops.CallbackData(
				chatops.DecisionApprove, id)},
			{"text": "Deny", "callback_data": chatops.CallbackData(chatops.DecisionDeny, id)},
		}}}
	}
	return s.call(ctx, ch, "sendMessage", msg)
}

// call invokes one Bot API method for the channel's chat. Errors never
// carry the bot token, which is part of the request path.
func (s *TelegramSender) call(ctx context.Context, ch Channel, method string,
	msg map[string]any) error {
	token, chat := ch.Config["bot_token"], ch.Config["chat_id"]
	if token == "" || chat == "" {
		return fmt.Errorf("telegram channel %q: missing bot_token or chat_id", ch.Name)
	}
	if err := s.policy.ValidateURL(s.apiBase); err != nil {
		return fmt.Errorf("telegram channel %q: %w", ch.Name, err)
	}
	if method != "answerCallbackQuery" {
		msg["chat_id"] = chat
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("telegram channel %q: encode: %w", ch.Name, err)
	}
	err = s.post(ctx, s.apiBase+"/bot"+token+"/"+method, body)
	if err != nil {
		redacted := strings.ReplaceAll(RedactError(err).Error(), token, "[redacted]")
		return fmt.Errorf("telegram channel %q: %s", ch.Name, redacted)
	}
	return nil
}

func (s *TelegramSender) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}
	return nil
}
