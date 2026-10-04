package notify

import (
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// withCardButtons appends an approval card's Approve / Reject / Snooze
// buttons to a Slack payload. Each carries only the card token; the
// callback is verified and the decider resolved server side.
func withCardButtons(payload []byte, token string) ([]byte, error) {
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
	msg["blocks"] = append(blocks, map[string]any{"type": "actions",
		"elements": []any{approve,
			button("Reject", chatops.SlackCardRejectAction, "danger"),
			button("Snooze 4h", chatops.SlackCardSnoozeAction, "")}})
	return json.Marshal(msg)
}

// cardKeyboard is an approval card's Telegram inline keyboard.
func cardKeyboard(token string) map[string]any {
	key := func(text string, d chatops.Decision) map[string]string {
		return map[string]string{"text": text,
			"callback_data": chatops.CardCallbackData(d, token)}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		key("Approve", chatops.DecisionApprove), key("Reject", chatops.DecisionDeny),
		key("Snooze 4h", chatops.DecisionSnooze),
	}}}
}
