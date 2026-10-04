package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// Fact cards (roadmap 2.3). A fact the model or a detector proposed goes
// to the channels that take approvals, as an approval_needed event with
// Confirm / Reject buttons. The dispatcher mints one single-use fact token
// per interactive Slack or Telegram channel, like approval-card tokens.

// Event data keys of fact cards.
const (
	dataFactCard = "fact_card"
	// DataFactToken is the per-channel fact card token a sender renders as
	// buttons.
	DataFactToken = "fact_card_token"
)

// FactCardRef identifies the fact a card decides and the content hash the
// decision is bound to.
type FactCardRef struct {
	Database  string    `json:"database"`
	FactID    int64     `json:"fact_id"`
	FactHash  string    `json:"fact_hash"`
	Title     string    `json:"title"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FactTokenIssuer mints and revokes per-channel fact card tokens.
type FactTokenIssuer interface {
	Issue(ctx context.Context, ch Channel, ref FactCardRef) (string, error)
	Revoke(ctx context.Context, token string) error
}

// FactProposedEvent is the card of one proposed fact; body carries the
// evidence.
func FactProposedEvent(ref FactCardRef, body string) Event {
	return Event{Type: "approval_needed", Severity: "info",
		Subject: "Confirm a fact: " + ref.Title, Body: body,
		Data: map[string]any{dataFactCard: ref, "database": ref.Database,
			"fact_id": ref.FactID, "title": ref.Title},
		DedupKey: fmt.Sprintf("fact:%s:%d", ref.Database, ref.FactID)}
}

// FactCardOf returns the fact card an event carries.
func FactCardOf(evt Event) (FactCardRef, bool) {
	ref, ok := evt.Data[dataFactCard].(FactCardRef)
	return ref, ok && ref.FactID > 0
}

func factToken(evt Event) string {
	token, _ := evt.Data[DataFactToken].(string)
	return token
}

// WithFactTokens makes the dispatcher mint a fact token for every
// interactive channel a fact card goes to.
func (d *Dispatcher) WithFactTokens(issuer FactTokenIssuer) *Dispatcher {
	d.factCards = issuer
	return d
}

// withFactToken mints the channel's token for a fact card event. A failure
// is logged and the card goes out without buttons (it can still be decided
// in the UI); the returned revoke undoes the token if the send fails.
func (d *Dispatcher) withFactToken(ctx context.Context, ch Channel, evt Event) (Event,
	func()) {
	ref, ok := FactCardOf(evt)
	if !ok || d.factCards == nil || !CardInteractive(ch) {
		return evt, func() {}
	}
	token, err := d.factCards.Issue(ctx, ch, ref)
	if err != nil {
		d.logFn("WARN", "fact card %d for channel %d sent without buttons: %v",
			ref.FactID, ch.ID, err)
		return evt, func() {}
	}
	out := evt
	out.Data = maps.Clone(evt.Data)
	out.Data[DataFactToken] = token
	return out, func() {
		if err := d.factCards.Revoke(ctx, token); err != nil {
			d.logFn("WARN", "revoke undelivered fact card %d: %v", ref.FactID, err)
		}
	}
}

// withFactButtons appends a fact card's Confirm / Reject buttons to a
// Slack payload.
func withFactButtons(payload []byte, token string) ([]byte, error) {
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, err
	}
	blocks, _ := msg["blocks"].([]any)
	button := func(text, actionID, style string) map[string]any {
		return map[string]any{"type": "button", "action_id": actionID, "value": token,
			"style": style, "text": map[string]any{"type": "plain_text", "text": text}}
	}
	msg["blocks"] = append(blocks, map[string]any{"type": "actions",
		"elements": []any{button("Confirm", chatops.SlackFactConfirmAction, "primary"),
			button("Reject", chatops.SlackFactRejectAction, "danger")}})
	return json.Marshal(msg)
}

// factKeyboard is a fact card's Telegram inline keyboard.
func factKeyboard(token string) map[string]any {
	key := func(text string, d chatops.Decision) map[string]string {
		return map[string]string{"text": text,
			"callback_data": chatops.FactCallbackData(d, token)}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		key("Confirm", chatops.DecisionApprove), key("Reject", chatops.DecisionDeny),
	}}}
}
