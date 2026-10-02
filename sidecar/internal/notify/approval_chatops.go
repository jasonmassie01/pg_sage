package notify

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// ActionApproval describes a Sage SRE proposal awaiting approval.
type ActionApproval struct {
	Database   string
	Title      string
	Summary    string
	Risk       string
	ProposalID string
	QueueID    int
}

// ActionApprovalEvent is the approval-needed event of a proposal. Chat
// channels that support it render Approve/Deny buttons carrying only the
// decision and the proposal id.
func ActionApprovalEvent(a ActionApproval) Event {
	return Event{
		Type:     "approval_needed",
		Severity: "warning",
		Subject:  fmt.Sprintf("Approval needed: %s", a.Title),
		Body: fmt.Sprintf("Database: %s\nRisk: %s\n%s", a.Database, a.Risk,
			a.Summary),
		Data: map[string]any{
			"title":                a.Title,
			"database":             a.Database,
			"risk":                 a.Risk,
			"approval_proposal_id": a.ProposalID,
			"queue_id":             a.QueueID,
		},
		DedupKey: "sre_proposal:" + a.ProposalID,
	}
}

// approvalProposal is the proposal id an event asks a decision on.
func approvalProposal(evt Event) (string, bool) {
	id, ok := evt.Data["approval_proposal_id"].(string)
	return id, ok && id != ""
}

// withApprovalButtons appends Approve/Deny buttons to a Slack payload
// when the event asks for a decision; other payloads are unchanged.
func withApprovalButtons(payload []byte, evt Event) ([]byte, error) {
	id, ok := approvalProposal(evt)
	if !ok {
		return payload, nil
	}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	blocks, _ := msg["blocks"].([]any)
	button := func(text, actionID, style string) map[string]any {
		return map[string]any{"type": "button", "action_id": actionID, "value": id,
			"style": style, "text": map[string]any{"type": "plain_text", "text": text}}
	}
	approve := button("Approve", chatops.SlackApproveAction, "primary")
	approve["confirm"] = map[string]any{
		"title": map[string]any{"type": "plain_text", "text": "Approve this action?"},
		"text": map[string]any{"type": "plain_text",
			"text": "pg_sage rechecks the target and runs it as you."},
		"confirm": map[string]any{"type": "plain_text", "text": "Approve"},
		"deny":    map[string]any{"type": "plain_text", "text": "Cancel"},
	}
	msg["blocks"] = append(blocks, map[string]any{"type": "actions",
		"elements": []any{approve, button("Deny", chatops.SlackDenyAction, "danger")}})
	return json.Marshal(msg)
}
