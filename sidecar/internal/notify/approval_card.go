package notify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// Approval cards (roadmap 1.5). The executor's approval request for a
// queued action names its queue item; the approval-card notifier adds the
// card (title, why, evidence, SQL, rollback, risk) and its reference; the
// dispatcher then mints one single-use card token per interactive Slack
// or Telegram channel, which the channel renders as Approve / Reject /
// Snooze buttons. Follow-ups carry the verification verdict.

// Event data keys of approval cards.
const (
	dataApprovalCard = "approval_card"
	// DataCardToken is the per-channel card token a sender renders as
	// buttons.
	DataCardToken = "approval_card_token"
	// DataReplyTo is the chat message a follow-up answers (Telegram).
	DataReplyTo = "reply_to_message_id"
)

// ErrChannelDisabled means a direct send targeted a disabled channel.
var ErrChannelDisabled = errors.New("notify: channel is disabled")

// CardRef identifies the card an event carries: the queue item it
// approves, the content hash the approval is bound to, and when it ends.
type CardRef struct {
	Database  string    `json:"database"`
	QueueID   int       `json:"queue_id"`
	CardHash  string    `json:"card_hash"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CardTokenIssuer mints and revokes per-channel card tokens.
type CardTokenIssuer interface {
	Issue(ctx context.Context, ch Channel, ref CardRef) (string, error)
	Revoke(ctx context.Context, token string) error
}

// QueuedApprovalEvent is the approval-needed event of a queued action: the
// legacy event plus its queue item, so a card can be built and decided.
func QueuedApprovalEvent(title, sql, database, risk string, queueID int) Event {
	evt := ApprovalNeededEvent(title, sql, database, risk)
	evt.Data["queue_id"] = queueID
	evt.DedupKey = fmt.Sprintf("approval:%s:%d", database, queueID)
	return evt
}

// WithApprovalCard returns a copy of evt whose body is the card text and
// which carries the card reference.
func WithApprovalCard(evt Event, ref CardRef, body string) Event {
	out := evt
	out.Data = maps.Clone(evt.Data)
	if out.Data == nil {
		out.Data = map[string]any{}
	}
	out.Data[dataApprovalCard] = ref
	out.Body = body
	return out
}

// ApprovalCardOf returns the card an event carries.
func ApprovalCardOf(evt Event) (CardRef, bool) {
	ref, ok := evt.Data[dataApprovalCard].(CardRef)
	return ref, ok && ref.QueueID > 0
}

// CardInteractive reports whether a channel can take card decisions: a
// Slack channel configured interactive with a signing secret, or a
// Telegram channel with a webhook secret (callbacks are verified with
// them; without one, buttons could never work).
func CardInteractive(ch Channel) bool {
	switch ch.Type {
	case "slack":
		return ch.Config["interactive"] == "true" && ch.Config["signing_secret"] != ""
	case "telegram":
		return ch.Config["webhook_secret"] != ""
	}
	return false
}

func cardToken(evt Event) string {
	token, _ := evt.Data[DataCardToken].(string)
	return token
}

// WithCardTokens makes the dispatcher mint a card token for every
// interactive channel an approval card goes to.
func (d *Dispatcher) WithCardTokens(issuer CardTokenIssuer) *Dispatcher {
	d.cards = issuer
	return d
}

// withCardToken mints the channel's token for a card event. A failure is
// logged and the card goes out without buttons (it can still be decided
// in the UI); the returned revoke undoes the token if the send fails.
func (d *Dispatcher) withCardToken(ctx context.Context, ch Channel, evt Event) (Event,
	func()) {
	ref, ok := ApprovalCardOf(evt)
	if !ok || d.cards == nil || !CardInteractive(ch) {
		return evt, func() {}
	}
	if _, sre := approvalProposal(evt); sre {
		return evt, func() {}
	}
	token, err := d.cards.Issue(ctx, ch, ref)
	if err != nil {
		d.logFn("WARN", "approval card %d for channel %d sent without buttons: %v",
			ref.QueueID, ch.ID, err)
		return evt, func() {}
	}
	out := evt
	out.Data = maps.Clone(evt.Data)
	out.Data[DataCardToken] = token
	return out, func() {
		if err := d.cards.Revoke(ctx, token); err != nil {
			d.logFn("WARN", "revoke undelivered approval card %d: %v", ref.QueueID, err)
		}
	}
}

// SendToChannel delivers evt to one enabled channel by id, bypassing rule
// matching: a follow-up goes where its card went.
func (d *Dispatcher) SendToChannel(ctx context.Context, channelID int, evt Event) error {
	ch, err := d.loadChannel(ctx, channelID)
	if err != nil {
		return fmt.Errorf("loading channel %d: %w", channelID, err)
	}
	if !ch.Enabled {
		return fmt.Errorf("channel %d: %w", channelID, ErrChannelDisabled)
	}
	sender, ok := d.senders[ch.Type]
	if !ok {
		return fmt.Errorf("no sender for type %q", ch.Type)
	}
	return d.deliver(ctx, sender, *ch, evt)
}

// ApprovalOutcome is the verification verdict of a card's action.
type ApprovalOutcome struct {
	Database  string
	QueueID   int
	Title     string
	Verdict   string
	Detail    string
	Predicted string
	ReplyTo   int64
}

var outcomeHeadings = map[string]string{"verified": "Verified",
	"rolled_back": "Rolled back", "regressed": "Regressed", "failed": "Failed",
	"unverifiable": "Unverifiable", "rejected": "Rejected", "expired": "Expired",
	"superseded": "Superseded", "blocked": "Blocked"}

// ApprovalOutcomeEvent is the follow-up of an approval card.
func ApprovalOutcomeEvent(o ApprovalOutcome) Event {
	heading, ok := outcomeHeadings[o.Verdict]
	if !ok {
		heading = "Closed"
	}
	severity := "info"
	if o.Verdict == "regressed" || o.Verdict == "rolled_back" || o.Verdict == "failed" {
		severity = "warning"
	}
	lines := []string{fmt.Sprintf("Database: %s | queue item %d", o.Database, o.QueueID)}
	if o.Detail != "" {
		lines = append(lines, "Result: "+o.Detail)
	}
	if o.Predicted != "" {
		lines = append(lines, "Predicted: "+o.Predicted)
	}
	evt := Event{Type: "approval_outcome", Severity: severity,
		Subject: heading + ": " + o.Title, Body: strings.Join(lines, "\n"),
		Data: map[string]any{"database": o.Database, "queue_id": o.QueueID,
			"verdict": o.Verdict, "title": o.Title},
		DedupKey: fmt.Sprintf("approval:%s:%d", o.Database, o.QueueID)}
	if o.ReplyTo > 0 {
		evt.Data[DataReplyTo] = o.ReplyTo
	}
	return evt
}
