package store

import (
	"errors"
	"testing"
)

// ChatOps channels: Telegram is a channel type (bot token + chat id), and
// a Slack channel that offers approval buttons must carry the signing
// secret its callbacks are verified with.

func TestTelegramChannelValidation(t *testing.T) {
	if err := validateChannelType("telegram"); err != nil {
		t.Fatalf("telegram type rejected: %v", err)
	}
	ok := map[string]string{"bot_token": "123456:ABC", "chat_id": "-1001234"}
	if err := validateChannelConfig("telegram", ok); err != nil {
		t.Fatalf("valid telegram config rejected: %v", err)
	}
	if err := validateChannelConfig("telegram", map[string]string{"bot_token": "1:x",
		"chat_id": "@sage_ops"}); err != nil {
		t.Fatalf("channel username rejected: %v", err)
	}
	for name, cfg := range map[string]map[string]string{
		"no token":   {"chat_id": "-1001"},
		"no chat":    {"bot_token": "1:x"},
		"bad chat":   {"bot_token": "1:x", "chat_id": "ops room"},
		"bad token":  {"bot_token": "not a token", "chat_id": "-1001"},
		"bad secret": {"bot_token": "1:x", "chat_id": "-1001", "webhook_secret": "has space"},
	} {
		if err := validateChannelConfig("telegram", cfg); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: %v, want ErrValidation", name, err)
		}
	}
}

func TestInteractiveSlackNeedsASigningSecret(t *testing.T) {
	cfg := map[string]string{"webhook_url": "https://hooks.slack.com/services/x",
		"interactive": "true"}
	if err := validateChannelConfig("slack", cfg); !errors.Is(err, ErrValidation) {
		t.Fatalf("interactive slack without a signing secret = %v", err)
	}
	cfg["signing_secret"] = "8f742231b10e8888abcd99yyyzzz85a5"
	if err := validateChannelConfig("slack", cfg); err != nil {
		t.Fatalf("interactive slack with a secret rejected: %v", err)
	}
	cfg["interactive"] = "maybe"
	if err := validateChannelConfig("slack", cfg); !errors.Is(err, ErrValidation) {
		t.Fatalf("interactive=maybe = %v, want ErrValidation", err)
	}
}
