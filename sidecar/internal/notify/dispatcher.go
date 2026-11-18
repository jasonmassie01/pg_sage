package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	// plans mints plan-card tokens (plan-03b); nil: no buttons.
	plans PlanTokenIssuer
	// outbox queues rule deliveries for DrainOutbox; nil delivers at
	// once (alerting-06). policy governs finding alerts in the outbox.
	outbox   OutboxStore
	policyMu sync.RWMutex
	policy   *AlertPolicy
	now      func() time.Time
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
// Only a failure to load the rules is returned; per-rule failures are
// logged.
func (d *Dispatcher) Dispatch(
	ctx context.Context, event Event,
) error {
	_, err := d.Submit(ctx, event)
	return err
}

// Submit routes event to every matching rule and says what happened to
// each: queued in the outbox (when UseOutbox was called and the event
// can wait), delivered at once, refused or suppressed by the alert state.
func (d *Dispatcher) Submit(ctx context.Context, event Event) (DispatchResult, error) {
	res, _, err := d.SubmitWithOutcomes(ctx, event)
	return res, err
}

// SubmitWithOutcomes is Submit that also says what each matching rule's
// channel did (ux-02b), so a caller can record which channel failed.
func (d *Dispatcher) SubmitWithOutcomes(
	ctx context.Context, event Event,
) (DispatchResult, []ChannelOutcome, error) {
	var res DispatchResult
	rules, err := d.loadMatchingRules(ctx, event.Type)
	if err != nil {
		return res, nil, fmt.Errorf("loading rules: %w", err)
	}
	outs := make([]ChannelOutcome, 0, len(rules))
	for _, rule := range rules {
		d.submitRule(ctx, rule, event, &res, &outs)
	}
	return res, outs, nil
}

func (d *Dispatcher) submitRule(
	ctx context.Context, rule Rule, event Event, res *DispatchResult,
	outs *[]ChannelOutcome,
) {
	ch, sender, err := d.ruleTarget(ctx, rule, event)
	switch {
	case err != nil:
		d.logFn("ERROR", "rule %d dispatch: %v", rule.ID, err)
		res.Refused++
		*outs = append(*outs, ChannelOutcome{ChannelID: rule.ChannelID,
			Channel: fmt.Sprintf("channel %d", rule.ChannelID), Status: OutcomeRefused,
			Error: RedactError(err).Error()})
		return
	case ch == nil:
		return
	case d.outbox != nil && queueable(event):
		before := *res
		d.enqueue(ctx, *ch, event, res)
		*outs = append(*outs, ChannelOutcome{ChannelID: ch.ID,
			Channel: ch.Name, Status: queuedOutcome(before, *res)})
		return
	}
	out := ChannelOutcome{ChannelID: ch.ID, Channel: ch.Name, Status: OutcomeSent}
	if err := d.deliver(ctx, sender, *ch, event); err != nil {
		d.logFn("ERROR", "rule %d dispatch: %v", rule.ID, err)
		res.Failed++
		out.Status, out.Error = OutcomeError, err.Error()
	} else {
		res.Delivered++
	}
	*outs = append(*outs, out)
}

func (d *Dispatcher) processRule(
	ctx context.Context, rule Rule, event Event,
) error {
	ch, sender, err := d.ruleTarget(ctx, rule, event)
	if err != nil || ch == nil {
		return err
	}
	return d.deliver(ctx, sender, *ch, event)
}

// ruleTarget is the channel and sender a rule delivers event to; a nil
// channel means the rule does not fire (severity below min, disabled).
func (d *Dispatcher) ruleTarget(
	ctx context.Context, rule Rule, event Event,
) (*Channel, Sender, error) {
	if !SeverityMeetsMin(event.Severity, rule.MinSeverity) {
		// A debug line, not a log row: filtered events recur every cycle
		// and would flood notification_log (alerting-03).
		d.logFn("DEBUG", "rule %d skipped %s event: severity %s below min_severity %s",
			rule.ID, event.Type, event.Severity, rule.MinSeverity)
		return nil, nil, nil
	}
	ch, err := d.loadChannel(ctx, rule.ChannelID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading channel %d: %w",
			rule.ChannelID, err)
	}
	if !ch.Enabled {
		return nil, nil, nil
	}
	sender, ok := d.senders[ch.Type]
	if !ok {
		return nil, nil, d.logDelivery(ctx, ch.ID, event, "error",
			fmt.Sprintf("no sender for type %q", ch.Type))
	}
	return ch, sender, nil
}

// deliver sends and records the attempt with a redacted error string.
func (d *Dispatcher) deliver(
	ctx context.Context, sender Sender, ch Channel, event Event,
) error {
	event, revoke := d.withCardToken(ctx, ch, event)
	event, revokeFact := d.withFactToken(ctx, ch, event)
	event, revokePlan := d.withPlanToken(ctx, ch, event)
	messageID, sendErr := send(ctx, sender, ch, event)
	sendErr = RedactError(sendErr)
	rec := Delivery{ChannelID: ch.ID, Event: event, Status: "sent",
		ProviderMessageID: messageID}
	if sendErr != nil {
		revoke()
		revokeFact()
		revokePlan()
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
// the store keeps it, and returns the send failure. A failed log write is
// logged with the event and channel and never fails the delivery.
func (d *Dispatcher) recordDelivery(ctx context.Context, rec Delivery) error {
	var err error
	if r, ok := d.store.(DeliveryRecorder); ok {
		err = r.RecordDelivery(ctx, rec)
	} else {
		err = d.store.LogDelivery(ctx, rec.ChannelID, rec.Event, rec.Status, rec.Error)
	}
	if err != nil {
		d.logFn("ERROR", "record %s delivery (database %q) to channel %d: %v",
			rec.Event.Type, logDatabase(rec.Event), rec.ChannelID, err)
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
