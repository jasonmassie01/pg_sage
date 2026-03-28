package notify

import (
	"context"
	"fmt"
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
	pool    *pgxpool.Pool
	senders map[string]Sender
	logFn   func(string, string, ...any)
}

// NewDispatcher creates a Dispatcher.
func NewDispatcher(
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) *Dispatcher {
	return &Dispatcher{
		pool:    pool,
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

	sendErr := sender.Send(ctx, *ch, event)
	status := "sent"
	errMsg := ""
	if sendErr != nil {
		status = "error"
		errMsg = sendErr.Error()
	}
	return d.logDelivery(ctx, ch.ID, event, status, errMsg)
}

func (d *Dispatcher) loadMatchingRules(
	ctx context.Context, eventType string,
) ([]Rule, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := d.pool.Query(qctx,
		`SELECT id, channel_id, event, min_severity
		 FROM sage.notification_rules
		 WHERE event = $1 AND enabled = true`, eventType)
	if err != nil {
		return nil, fmt.Errorf("query rules: %w", err)
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(
			&r.ID, &r.ChannelID, &r.Event, &r.MinSeverity,
		); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.Enabled = true
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (d *Dispatcher) loadChannel(
	ctx context.Context, id int,
) (*Channel, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ch Channel
	var cfgJSON []byte
	err := d.pool.QueryRow(qctx,
		`SELECT id, name, type, config, enabled
		 FROM sage.notification_channels
		 WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled)
	if err != nil {
		return nil, fmt.Errorf("get channel %d: %w", id, err)
	}
	ch.Config = parseConfig(cfgJSON)
	return &ch, nil
}

func (d *Dispatcher) logDelivery(
	ctx context.Context,
	channelID int, event Event,
	status, errMsg string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := d.pool.Exec(qctx,
		`INSERT INTO sage.notification_log
		    (channel_id, event, subject, body, status, error)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		channelID, event.Type, event.Subject,
		event.Body, status, errMsg)
	if err != nil {
		d.logFn("ERROR", "insert notification_log: %v", err)
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

	sendErr := sender.Send(ctx, ch, event)
	status := "sent"
	errMsg := ""
	if sendErr != nil {
		status = "error"
		errMsg = sendErr.Error()
	}
	return d.logDelivery(ctx, ch.ID, event, status, errMsg)
}
