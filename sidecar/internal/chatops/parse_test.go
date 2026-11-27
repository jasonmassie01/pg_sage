package chatops

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// Callback payloads carry only a decision and a card token; everything
// else (who, which team or chat, a replay nonce) comes from the
// provider's envelope. Anything ambiguous or unknown is malformed. The
// retired proposal buttons ("sage_approve" / "sage:a:<proposal id>") are
// still recognised for one release, only to answer that they are outdated
// (policy-08).

const proposalID = "33333333-3333-4333-8333-333333333333"

func slackForm(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return []byte(url.Values{"payload": {string(raw)}}.Encode())
}

func slackPayload(actionID, value string) map[string]any {
	return map[string]any{
		"type": "block_actions", "team": map[string]any{"id": "T0001"},
		"user":         map[string]any{"id": "U0042", "username": "alice", "team_id": "T0001"},
		"response_url": "https://hooks.slack.com/actions/T0001/1/abc",
		"actions": []any{map[string]any{"action_id": actionID, "value": value,
			"action_ts": "1759320000.123456", "type": "button"}},
	}
}

func TestParseSlackLegacyProposalButtonIsOutdated(t *testing.T) {
	for actionID, want := range map[string]Decision{
		LegacySlackApproveAction: DecisionApprove, LegacySlackDenyAction: DecisionDeny,
	} {
		got, err := ParseSlack(slackForm(t, slackPayload(actionID, proposalID)))
		if err != nil {
			t.Fatalf("%s: %v", actionID, err)
		}
		if got.Provider != ProviderSlack || got.TeamID != "T0001" || got.UserID != "U0042" ||
			got.UserName != "alice" || got.Decision != want || !got.Outdated ||
			got.CardToken != "" || got.FactToken != "" ||
			got.ResponseURL != "https://hooks.slack.com/actions/T0001/1/abc" ||
			got.Nonce != "T0001:U0042:1759320000.123456" {
			t.Fatalf("%s parsed as %+v", actionID, got)
		}
	}
}

func TestParseSlackRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"not a form":       []byte("%zz"),
		"no payload":       []byte("foo=bar"),
		"payload not json": []byte(url.Values{"payload": {"{"}}.Encode()),
		"other type": slackForm(t, func() map[string]any {
			p := slackPayload(LegacySlackApproveAction, proposalID)
			p["type"] = "view_submission"
			return p
		}()),
		"unknown action": slackForm(t, slackPayload("sage_terminate", proposalID)),
		"empty value":    slackForm(t, slackPayload(LegacySlackDenyAction, "")),
		"bad proposal id": slackForm(t, slackPayload(LegacySlackApproveAction,
			"'; DROP TABLE x; --")),
		"no actions": slackForm(t, func() map[string]any {
			p := slackPayload(LegacySlackApproveAction, proposalID)
			p["actions"] = []any{}
			return p
		}()),
		"two actions": slackForm(t, func() map[string]any {
			p := slackPayload(LegacySlackApproveAction, proposalID)
			p["actions"] = append(p["actions"].([]any), p["actions"].([]any)[0])
			return p
		}()),
		"no user": slackForm(t, func() map[string]any {
			p := slackPayload(LegacySlackApproveAction, proposalID)
			delete(p, "user")
			return p
		}()),
		"no team": slackForm(t, func() map[string]any {
			p := slackPayload(LegacySlackApproveAction, proposalID)
			delete(p, "team")
			return p
		}()),
	}
	for name, body := range cases {
		if _, err := ParseSlack(body); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: ParseSlack = %v, want ErrMalformed", name, err)
		}
	}
}

func telegramUpdate(data string) []byte {
	raw, _ := json.Marshal(map[string]any{"update_id": 987654,
		"callback_query": map[string]any{"id": "cbq-1",
			"from":    map[string]any{"id": 12345, "username": "bob"},
			"message": map[string]any{"message_id": 77, "chat": map[string]any{"id": -1001}},
			"data":    data}})
	return raw
}

func TestParseTelegramLegacyProposalButtonIsOutdated(t *testing.T) {
	for data, want := range map[string]Decision{"sage:a:" + proposalID: DecisionApprove,
		"sage:d:" + strings.ToUpper(proposalID): DecisionDeny} {
		got, err := ParseTelegram(telegramUpdate(data))
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if got.Provider != ProviderTelegram || got.UserID != "12345" ||
			got.UserName != "bob" || got.ChatID != "-1001" || got.CallbackID != "cbq-1" ||
			got.MessageID != 77 || got.Decision != want || !got.Outdated ||
			got.CardToken != "" || got.Nonce != "update:987654" || got.TeamID != "" {
			t.Fatalf("%s parsed as %+v", data, got)
		}
	}
}

func TestParseTelegramIgnoresOtherUpdates(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"update_id": 1,
		"message": map[string]any{"text": "/approve all"}})
	if _, err := ParseTelegram(raw); !errors.Is(err, ErrNotCallback) {
		t.Fatalf("a chat message = %v, want ErrNotCallback", err)
	}
}

func TestParseTelegramRejectsMalformed(t *testing.T) {
	for name, body := range map[string][]byte{
		"not json":   []byte("{"),
		"bad prefix": telegramUpdate("approve:" + proposalID),
		"bad verb":   telegramUpdate("sage:t:" + proposalID),
		"bad id":     telegramUpdate("sage:a:not-a-uuid"),
		"no update id": []byte(`{"callback_query":{"id":"x","from":{"id":1},` +
			`"data":"sage:a:` + proposalID + `"}}`),
		"no sender": []byte(`{"update_id":2,"callback_query":{"id":"x","data":"sage:a:` +
			proposalID + `"}}`),
	} {
		if _, err := ParseTelegram(body); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: ParseTelegram = %v, want ErrMalformed", name, err)
		}
	}
}

// A current card button is never read as an outdated one.
func TestParseCardButtonsAreNotOutdated(t *testing.T) {
	token := "AbCdEfGhIjKlMnOpQrStUv"
	slack, err := ParseSlack(slackForm(t, slackPayload(SlackCardApproveAction, token)))
	if err != nil || slack.Outdated || slack.CardToken != token {
		t.Fatalf("slack card button = %+v, %v", slack, err)
	}
	tg, err := ParseTelegram(telegramUpdate(CardCallbackData(DecisionDeny, token)))
	if err != nil || tg.Outdated || tg.CardToken != token || tg.Decision != DecisionDeny {
		t.Fatalf("telegram card button = %+v, %v", tg, err)
	}
}
