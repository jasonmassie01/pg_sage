package notify

import (
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// withCardButtons appends an approval card's Approve (unless the card is
// above the chat approval cap), Reject and (unless the card has none)
// Snooze buttons to a Slack payload. Each carries only the card token; the
// callback is verified and the decider resolved server side.
func withCardButtons(payload []byte, evt Event) ([]byte, error) {
	token, ref := cardToken(evt), cardRefOf(evt)
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
	elements := []any{button("Reject", chatops.SlackCardRejectAction, "danger")}
	if !ref.NoApprove {
		elements = append([]any{approve}, elements...)
	}
	if !ref.NoSnooze {
		elements = append(elements, button("Snooze 4h", chatops.SlackCardSnoozeAction, ""))
	}
	msg["blocks"] = append(blocks, map[string]any{"type": "actions", "elements": elements})
	return json.Marshal(msg)
}

// cardRefOf is the card an event carries (zero when none).
func cardRefOf(evt Event) CardRef {
	ref, _ := ApprovalCardOf(evt)
	return ref
}

// cardKeyboard is an approval card's Telegram inline keyboard.
func cardKeyboard(token string, ref CardRef) map[string]any {
	key := func(text string, d chatops.Decision) map[string]string {
		return map[string]string{"text": text,
			"callback_data": chatops.CardCallbackData(d, token)}
	}
	row := []map[string]string{key("Reject", chatops.DecisionDeny)}
	if !ref.NoApprove {
		row = append([]map[string]string{key("Approve", chatops.DecisionApprove)}, row...)
	}
	if !ref.NoSnooze {
		row = append(row, key("Snooze 4h", chatops.DecisionSnooze))
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{row}}
}
