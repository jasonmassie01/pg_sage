package notify

import (
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// withCardButtons appends an approval card's Approve / Reject (and, unless
// the card has none, Snooze) buttons to a Slack payload. Each carries only
// the card token; the callback is verified and the decider resolved
// server side.
func withCardButtons(payload []byte, token string, snooze bool) ([]byte, error) {
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	blocks, _ := msg["blocks"].([]any)
	button := func(text, actionID, style string) map[string]any {
		b := map[string]any{"type": "button", "action_id": actionID, "value": token,
			"text": map[string]any{"type": "plain_text", "text": text}}
		if style != "" {
			b["style"] = style
		}
		return b
	}
	approve := button("Approve", chatops.SlackCardApproveAction, "primary")
	approve["confirm"] = map[string]any{
		"title": map[string]any{"type": "plain_text", "text": "Approve this action?"},
		"text": map[string]any{"type": "plain_text",
			"text": "pg_sage rechecks policy and the exact SQL, then runs it as you."},
		"confirm": map[string]any{"type": "plain_text", "text": "Approve"},
		"deny":    map[string]any{"type": "plain_text", "text": "Cancel"},
	}
	elements := []any{approve, button("Reject", chatops.SlackCardRejectAction, "danger")}
	if snooze {
		elements = append(elements, button("Snooze 4h", chatops.SlackCardSnoozeAction, ""))
	}
	msg["blocks"] = append(blocks, map[string]any{"type": "actions", "elements": elements})
	return json.Marshal(msg)
}

// cardKeyboard is an approval card's Telegram inline keyboard.
// cardSnooze reports whether a card event offers Snooze.
func cardSnooze(evt Event) bool {
	ref, _ := ApprovalCardOf(evt)
	return !ref.NoSnooze
}

func cardKeyboard(token string, snooze bool) map[string]any {
	key := func(text string, d chatops.Decision) map[string]string {
		return map[string]string{"text": text,
			"callback_data": chatops.CardCallbackData(d, token)}
	}
	row := []map[string]string{key("Approve", chatops.DecisionApprove),
		key("Reject", chatops.DecisionDeny)}
	if snooze {
		row = append(row, key("Snooze 4h", chatops.DecisionSnooze))
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{row}}
}
