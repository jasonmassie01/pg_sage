package notify

import "context"

// Delivery is one recorded delivery attempt (sage.notification_log).
type Delivery struct {
	ChannelID int
	Event     Event
	Status    string // sent or error
	// Error is the redacted send failure; empty when sent.
	Error string
	// ProviderMessageID is the id the provider gave the sent message
	// (Telegram message_id), so a follow-up can reply to it (sre-08).
	ProviderMessageID string
}

// logDatabase is the database a delivery of evt is logged under: "" (NULL
// in sage.notification_log) for a fleet-wide event, the "all" selector
// included, so the log never names "all" as a database (alerting-06c).
func logDatabase(evt Event) string {
	if db := eventDatabase(evt); db != "all" {
		return db
	}
	return ""
}

// DeliveryRecorder is a RuleStore that also records the event's RefID and
// the provider's message id. A store without it records the attempt
// through LogDelivery alone.
type DeliveryRecorder interface {
	RecordDelivery(ctx context.Context, d Delivery) error
}

// MessageIDSender is a Sender that reports the id the provider gave the
// sent message. An empty id with a nil error is a sent message whose id
// is unknown.
type MessageIDSender interface {
	SendMessageID(ctx context.Context, ch Channel, evt Event) (string, error)
}

// send delivers evt through sender, keeping the provider's message id
// when the sender reports one.
func send(ctx context.Context, sender Sender, ch Channel, evt Event) (string, error) {
	if s, ok := sender.(MessageIDSender); ok {
		return s.SendMessageID(ctx, ch, evt)
	}
	return "", sender.Send(ctx, ch, evt)
}
