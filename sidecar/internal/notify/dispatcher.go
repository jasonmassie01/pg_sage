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
	sendErr := redactErr(sender.Send(ctx, ch, event))
	status, errMsg := "sent", ""
	if sendErr != nil {
		status, errMsg = "error", sendErr.Error()
	}
	return d.logDelivery(ctx, ch.ID, event, status, errMsg)
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
	if err := d.store.LogDelivery(ctx, channelID, event, status, errMsg); err != nil {
		d.logFn("ERROR", "%v", err)
	}
	if errMsg != "" {
		return fmt.Errorf("send failed: %s", errMsg)
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
