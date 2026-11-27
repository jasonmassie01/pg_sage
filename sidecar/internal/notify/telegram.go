package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// Send posts the event to the channel's chat, with the card's buttons
// when the event carries a card or fact token.
func (s *TelegramSender) Send(ctx context.Context, ch Channel, evt Event) error {
	_, err := s.SendMessageID(ctx, ch, evt)
	return err
}

// SendMessageID sends like Send and returns the message_id Telegram gave
// the message, so a follow-up can reply to it (sre-08). A sent message
// whose reply carries no usable id returns an empty id and no error.
func (s *TelegramSender) SendMessageID(ctx context.Context, ch Channel,
	evt Event) (string, error) {
	raw, err := s.callResult(ctx, ch, "sendMessage", telegramMessage(evt))
	if err != nil {
		return "", err
	}
	var reply struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Result.MessageID <= 0 {
		return "", nil
	}
	return strconv.FormatInt(reply.Result.MessageID, 10), nil
}

func telegramMessage(evt Event) map[string]any {
	text := evt.Subject
	if evt.Body != "" {
		text += "\n\n" + evt.Body
	}
	msg := map[string]any{"text": TruncateRunes(text, telegramTextMax),
		"disable_web_page_preview": true}
	if planToken(evt) != "" {
		msg["reply_markup"] = planKeyboard(evt)
	} else if token := factToken(evt); token != "" {
		msg["reply_markup"] = factKeyboard(token)
	} else if token := cardToken(evt); token != "" {
		msg["reply_markup"] = cardKeyboard(token, cardRefOf(evt))
	}
	if replyTo, ok := evt.Data[DataReplyTo].(int64); ok && replyTo > 0 {
		msg["reply_to_message_id"] = replyTo
		msg["allow_sending_without_reply"] = true
	}
	return msg
}

// Reply answers a decision made in the chat: it acknowledges the button
// press, then posts the outcome to the channel's chat.
func (s *TelegramSender) Reply(ctx context.Context, ch Channel, a chatops.Action,
	text string) error {
	if a.CallbackID != "" {
		if err := s.call(ctx, ch, "answerCallbackQuery", map[string]any{
			"callback_query_id": a.CallbackID}); err != nil {
			return err
		}
	}
	return s.call(ctx, ch, "sendMessage", map[string]any{
		"text": TruncateRunes(text, telegramTextMax), "disable_web_page_preview": true})
}

// call invokes one Bot API method for the channel's chat.
func (s *TelegramSender) call(ctx context.Context, ch Channel, method string,
	msg map[string]any) error {
	_, err := s.callResult(ctx, ch, method, msg)
	return err
}

// callResult invokes one Bot API method and returns the reply body (at
// most 64 KiB). Errors never carry the bot token, which is part of the
// request path.
func (s *TelegramSender) callResult(ctx context.Context, ch Channel, method string,
	msg map[string]any) ([]byte, error) {
	token, chat := ch.Config["bot_token"], ch.Config["chat_id"]
	if token == "" || chat == "" {
		return nil, fmt.Errorf("telegram channel %q: missing bot_token or chat_id", ch.Name)
	}
	if err := s.policy.ValidateURL(s.apiBase); err != nil {
		return nil, fmt.Errorf("telegram channel %q: %w", ch.Name, err)
	}
	if method != "answerCallbackQuery" {
		msg["chat_id"] = chat
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("telegram channel %q: encode: %w", ch.Name, err)
	}
	raw, err := s.post(ctx, s.apiBase+"/bot"+token+"/"+method, body)
	if err != nil {
		redacted := strings.ReplaceAll(RedactError(err).Error(), token, "[redacted]")
		return nil, fmt.Errorf("telegram channel %q: %s", ch.Name, redacted)
	}
	return raw, nil
}

func (s *TelegramSender) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram returned status %d", resp.StatusCode)
	}
	return raw, nil
}
