package notify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sender delivers a notification to a specific channel type.
type Sender interface {
	Send(ctx context.Context, channel Channel, event Event) error
	Type() string
}

// Dispatcher routes events to matching channels via registered senders.
type Dispatcher struct {
	store   RuleStore
	senders map[string]Sender
	logFn   func(string, string, ...any)
	// cards mints approval-card tokens; nil sends cards without buttons.
	cards CardTokenIssuer
	// factCards mints fact-card tokens (roadmap 2.3); nil: no buttons.
	factCards FactTokenIssuer
}

// NewDispatcher creates a Dispatcher that reads rules and channels from
// pool. In fleet / meta-DB mode pass the control-plane pool (or use
// NewDispatcherWithStore) — rules live where the UI writes them.
func NewDispatcher(
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) *Dispatcher {
	return NewDispatcherWithStore(NewPoolStore(pool, nil), logFn)
}

// NewDispatcherWithStore creates a Dispatcher over an explicit store
// (G7-B05).
func NewDispatcherWithStore(
	store RuleStore, logFn func(string, string, ...any),
) *Dispatcher {
	return &Dispatcher{
		store:   store,
		senders: make(map[string]Sender),
		logFn:   logFn,
	}
}

// RegisterSender adds a sender for a channel type.
func (d *Dispatcher) RegisterSender(s Sender) {
	d.senders[s.Type()] = s
}

// Dispatch sends a notification event to all matching channels/rules.
func (d *Dispatcher) Dispatch(
	ctx context.Context, event Event,
) error {
	rules, err := d.loadMatchingRules(ctx, event.Type)
	if err != nil {
		return fmt.Errorf("loading rules: %w", err)
	}

	for _, rule := range rules {
		if err := d.processRule(ctx, rule, event); err != nil {
			d.logFn("ERROR", "rule %d dispatch: %v",
				rule.ID, err)
		}
	}
	return nil
}

func (d *Dispatcher) processRule(
	ctx context.Context, rule Rule, event Event,
) error {
	if !SeverityMeetsMin(event.Severity, rule.MinSeverity) {
		// A debug line, not a log row: filtered events recur every cycle
		// and would flood notification_log (alerting-03).
		d.logFn("DEBUG", "rule %d skipped %s event: severity %s below min_severity %s",
			rule.ID, event.Type, event.Severity, rule.MinSeverity)
		return nil
	}

	ch, err := d.loadChannel(ctx, rule.ChannelID)
	if err != nil {
		return fmt.Errorf("loading channel %d: %w",
			rule.ChannelID, err)
	}
	if !ch.Enabled {
		return nil
	}

	sender, ok := d.senders[ch.Type]
	if !ok {
		return d.logDelivery(ctx, ch.ID, event, "error",
			fmt.Sprintf("no sender for type %q", ch.Type))
	}
	return d.deliver(ctx, sender, *ch, event)
}

// deliver sends and records the attempt with a redacted error string.
func (d *Dispatcher) deliver(
	ctx context.Context, sender Sender, ch Channel, event Event,
) error {
	event, revoke := d.withCardToken(ctx, ch, event)
	event, revokeFact := d.withFactToken(ctx, ch, event)
	messageID, sendErr := send(ctx, sender, ch, event)
	sendErr = RedactError(sendErr)
	rec := Delivery{ChannelID: ch.ID, Event: event, Status: "sent",
		ProviderMessageID: messageID}
	if sendErr != nil {
		revoke()
		revokeFact()
		rec.Status, rec.Error, rec.ProviderMessageID = "error", sendErr.Error(), ""
	}
	return d.recordDelivery(ctx, rec)
}

func (d *Dispatcher) loadMatchingRules(
	ctx context.Context, eventType string,
) ([]Rule, error) {
	return d.store.MatchingRules(ctx, eventType)
}

func (d *Dispatcher) loadChannel(
	ctx context.Context, id int,
) (*Channel, error) {
	return d.store.Channel(ctx, id)
}

func (d *Dispatcher) logDelivery(
	ctx context.Context,
	channelID int, event Event,
	status, errMsg string,
) error {
	return d.recordDelivery(ctx, Delivery{ChannelID: channelID, Event: event,
		Status: status, Error: errMsg})
}

// recordDelivery records one attempt, with the provider's message id when
// the store keeps it, and returns the send failure.
func (d *Dispatcher) recordDelivery(ctx context.Context, rec Delivery) error {
	var err error
	if r, ok := d.store.(DeliveryRecorder); ok {
		err = r.RecordDelivery(ctx, rec)
	} else {
		err = d.store.LogDelivery(ctx, rec.ChannelID, rec.Event, rec.Status, rec.Error)
	}
	if err != nil {
		d.logFn("ERROR", "%v", err)
	}
	if rec.Error != "" {
		return fmt.Errorf("send failed: %s", rec.Error)
	}
	return nil
}

// SendDirect sends an event through a specific channel, bypassing
// rule matching. Used for test notifications.
func (d *Dispatcher) SendDirect(
	ctx context.Context, ch Channel, event Event,
) error {
	sender, ok := d.senders[ch.Type]
	if !ok {
		return fmt.Errorf("no sender for type %q", ch.Type)
	}
	return d.deliver(ctx, sender, ch, event)
}
