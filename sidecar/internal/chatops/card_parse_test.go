package chatops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Approval-card buttons carry only a decision and an opaque card token
// (22 base64url characters). The token names one card sent to one
// channel; who decided still comes from the provider envelope.

const cardToken = "AbCdEfGhIjKlMnOpQrStUv"

func TestParseSlackCardDecisions(t *testing.T) {
	for actionID, want := range map[string]Decision{
		SlackCardApproveAction: DecisionApprove,
		SlackCardRejectAction:  DecisionDeny,
		SlackCardSnoozeAction:  DecisionSnooze,
	} {
		got, err := ParseSlack(slackForm(t, slackPayload(actionID, cardToken)))
		if err != nil {
			t.Fatalf("%s: %v", actionID, err)
		}
		if got.Decision != want || got.CardToken != cardToken || got.ProposalID != "" ||
			got.TeamID != "T0001" || got.UserID != "U0042" ||
			got.Nonce != "T0001:U0042:1759320000.123456" {
			t.Fatalf("%s parsed as %+v", actionID, got)
		}
	}
}

func TestParseSlackCardRejectsBadTokens(t *testing.T) {
	for name, value := range map[string]string{
		"empty":        "",
		"too short":    cardToken[:21],
		"too long":     cardToken + "x",
		"bad alphabet": "AbCdEfGhIjKlMnOpQrSt+/",
		"a proposal":   proposalID,
		"padding":      "AbCdEfGhIjKlMnOpQrSt==",
	} {
		_, err := ParseSlack(slackForm(t, slackPayload(SlackCardApproveAction, value)))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	// A card token on a legacy proposal button is not a proposal id.
	if _, err := ParseSlack(slackForm(t, slackPayload(SlackApproveAction,
		cardToken))); !errors.Is(err, ErrMalformed) {
		t.Fatalf("legacy button with a card token: err = %v", err)
	}
}

func cardTelegramUpdate(data string) []byte {
	raw, _ := json.Marshal(map[string]any{"update_id": 77,
		"callback_query": map[string]any{"id": "cbq-1",
			"from":    map[string]any{"id": 4242, "username": "bob"},
			"message": map[string]any{"message_id": 9001, "chat": map[string]any{"id": -1001}},
			"data":    data}})
	return raw
}

func TestParseTelegramCardDecisions(t *testing.T) {
	for d, want := range map[Decision]Decision{
		DecisionApprove: DecisionApprove, DecisionDeny: DecisionDeny,
		DecisionSnooze: DecisionSnooze,
	} {
		data := CardCallbackData(d, cardToken)
		if len(data) > 64 {
			t.Fatalf("callback data %q exceeds Telegram's 64 bytes", data)
		}
		got, err := ParseTelegram(cardTelegramUpdate(data))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if got.Decision != want || got.CardToken != cardToken || got.ProposalID != "" ||
			got.ChatID != "-1001" || got.MessageID != 9001 || got.UserID != "4242" ||
			got.Nonce != "update:77" || got.CallbackID != "cbq-1" {
			t.Fatalf("%s parsed as %+v", d, got)
		}
	}
}

func TestParseTelegramCardRejectsMalformed(t *testing.T) {
	for name, data := range map[string]string{
		"unknown verb":  "sage:cx:" + cardToken,
		"short token":   "sage:ca:" + cardToken[:10],
		"bad alphabet":  "sage:ca:" + strings.Repeat("*", 22),
		"no token":      "sage:ca:",
		"wrong prefix":  "card:ca:" + cardToken,
		"proposal verb": "sage:a:" + cardToken,
	} {
		if _, err := ParseTelegram(cardTelegramUpdate(data)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestValidCardToken(t *testing.T) {
	if !ValidCardToken(cardToken) || !ValidCardToken("-_-_-_-_-_-_-_-_-_-_-_") {
		t.Fatal("valid tokens refused")
	}
	for _, bad := range []string{"", cardToken[:21], cardToken + "A", "AbCdEfGhIjKlMnOpQrSt.v"} {
		if ValidCardToken(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCardCallbackDataIsDistinctPerDecision(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range []Decision{DecisionApprove, DecisionDeny, DecisionSnooze} {
		data := CardCallbackData(d, cardToken)
		if seen[data] || !strings.HasSuffix(data, cardToken) {
			t.Fatalf("callback data %q", data)
		}
		seen[data] = true
	}
}
