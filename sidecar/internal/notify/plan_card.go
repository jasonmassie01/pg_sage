package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// Plan cards (plan-03b). A proposed plan goes to the channels that take
// approval cards, as an approval_needed event with the buttons its risk
// tiers allow from chat: Approve all, Approve up to step K, Approve step 1
// only, and always Reject. The dispatcher mints one single-use plan token
// per interactive Slack or Telegram channel, like approval-card tokens.

// Event data keys of plan cards.
const (
	dataPlanCard = "plan_card"
	// DataPlanToken is the per-channel plan card token a sender renders as
	// buttons.
	DataPlanToken = "plan_card_token"
)

// PlanCardRef identifies the plan a card decides, the plan hash the
// decision is bound to and the decisions the card offers.
type PlanCardRef struct {
	Database  string               `json:"database"`
	PlanID    int64                `json:"plan_id"`
	PlanHash  string               `json:"plan_hash"`
	Title     string               `json:"title"`
	Steps     int                  `json:"steps"`
	Offered   []chatops.PlanChoice `json:"offered"`
	UpTo      int                  `json:"up_to,omitempty"`
	ExpiresAt time.Time            `json:"expires_at"`
}

// PlanTokenIssuer mints and revokes per-channel plan card tokens.
type PlanTokenIssuer interface {
	Issue(ctx context.Context, ch Channel, ref PlanCardRef) (string, error)
	Revoke(ctx context.Context, token string) error
}

// PlanProposedEvent is the card of one proposed plan; body summarizes it.
// It asks a human to decide, so it carries the warning severity of every
// approval card. One card per plan hash: an edited plan is a new card.
func PlanProposedEvent(ref PlanCardRef, body string) Event {
	return Event{Type: "approval_needed", Severity: "warning",
		Subject: "Approve a plan: " + ref.Title, Body: body,
		Data: map[string]any{dataPlanCard: ref, "database": ref.Database,
			"plan_id": ref.PlanID, "title": ref.Title},
		DedupKey: fmt.Sprintf("plan:%s:%d:%s", ref.Database, ref.PlanID, ref.PlanHash)}
}

// PlanCardOf returns the plan card an event carries.
func PlanCardOf(evt Event) (PlanCardRef, bool) {
	ref, ok := evt.Data[dataPlanCard].(PlanCardRef)
	return ref, ok && ref.PlanID > 0
}

func planToken(evt Event) string {
	token, _ := evt.Data[DataPlanToken].(string)
	return token
}

// WithPlanTokens makes the dispatcher mint a plan token for every
// interactive channel a plan card goes to.
func (d *Dispatcher) WithPlanTokens(issuer PlanTokenIssuer) *Dispatcher {
	d.plans = issuer
	return d
}

// withPlanToken mints the channel's token for a plan card event. A failure
// is logged and the card goes out without buttons (it can still be decided
// in the app); the returned revoke undoes the token if the send fails.
func (d *Dispatcher) withPlanToken(ctx context.Context, ch Channel, evt Event) (Event,
	func()) {
	ref, ok := PlanCardOf(evt)
	if !ok || d.plans == nil || !CardInteractive(ch) {
		return evt, func() {}
	}
	token, err := d.plans.Issue(ctx, ch, ref)
	if err != nil {
		d.logFn("WARN", "plan card %d for channel %d sent without buttons: %v",
			ref.PlanID, ch.ID, err)
		return evt, func() {}
	}
	out := evt
	out.Data = maps.Clone(evt.Data)
	out.Data[DataPlanToken] = token
	return out, func() {
		if err := d.plans.Revoke(ctx, token); err != nil {
			d.logFn("WARN", "revoke undelivered plan card %d: %v", ref.PlanID, err)
		}
	}
}

// planButton is one plan card button: its decision, label and Slack id.
type planButton struct {
	choice   chatops.PlanChoice
	label    string
	actionID string
	style    string
}

// planButtons are the card's buttons in a fixed order, those it offers.
func planButtons(evt Event) []planButton {
	ref, _ := PlanCardOf(evt)
	all := []planButton{
		{chatops.PlanApproveAll, "Approve all", chatops.SlackPlanApproveAllAction, "primary"},
		{chatops.PlanApproveUpTo, fmt.Sprintf("Approve up to step %d", ref.UpTo),
			chatops.SlackPlanApproveUpToAction, "primary"},
		{chatops.PlanApproveNext, "Approve step 1 only", chatops.SlackPlanApproveNextAction,
			""},
		{chatops.PlanReject, "Reject", chatops.SlackPlanRejectAction, "danger"},
	}
	out := make([]planButton, 0, len(all))
	for _, b := range all {
		if slices.Contains(ref.Offered, b.choice) {
			out = append(out, b)
		}
	}
	return out
}

// withPlanButtons appends a plan card's buttons to a Slack payload.
func withPlanButtons(payload []byte, evt Event) ([]byte, error) {
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	blocks, _ := msg["blocks"].([]any)
	elements := []any{}
	for _, b := range planButtons(evt) {
		button := map[string]any{"type": "button", "action_id": b.actionID,
			"value": planToken(evt), "text": map[string]any{"type": "plain_text",
				"text": b.label}}
		if b.style != "" {
			button["style"] = b.style
		}
		elements = append(elements, button)
	}
	msg["blocks"] = append(blocks, map[string]any{"type": "actions", "elements": elements})
	return json.Marshal(msg)
}

// planKeyboard is a plan card's Telegram inline keyboard, one button a row.
func planKeyboard(evt Event) map[string]any {
	rows := [][]map[string]string{}
	for _, b := range planButtons(evt) {
		rows = append(rows, []map[string]string{{"text": b.label,
			"callback_data": chatops.PlanCallbackData(b.choice, planToken(evt))}})
	}
	return map[string]any{"inline_keyboard": rows}
}
