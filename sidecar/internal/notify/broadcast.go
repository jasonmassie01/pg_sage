package notify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EventSecurityBreakGlass is raised on every break-glass admin login (E1).
// It is broadcast to every enabled channel, so it never depends on an
// operator having written a rule for it.
const EventSecurityBreakGlass = "security_break_glass"

// ErrBroadcastUnsupported means the dispatcher's store cannot list channels.
var ErrBroadcastUnsupported = errors.New("notify: store cannot list channels for broadcast")

// ChannelLister is implemented by stores that can enumerate channels.
type ChannelLister interface {
	EnabledChannelIDs(ctx context.Context) ([]int, error)
}

// Broadcast sends a security event to every enabled channel, ignoring
// rules and severity floors, and records each attempt in the delivery log.
// It returns how many channels accepted the event. One failing channel
// does not stop the others.
func (d *Dispatcher) Broadcast(ctx context.Context, event Event) (int, error) {
	if d == nil {
		return 0, errors.New("notify: broadcast on a nil dispatcher")
	}
	lister, ok := d.store.(ChannelLister)
	if !ok {
		return 0, ErrBroadcastUnsupported
	}
	ids, err := lister.EnabledChannelIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing channels for broadcast: %w", err)
	}
	delivered := 0
	for _, id := range ids {
		if d.broadcastTo(ctx, id, event) {
			delivered++
		}
	}
	return delivered, nil
}

// broadcastTo delivers to one channel and reports whether it was sent.
func (d *Dispatcher) broadcastTo(ctx context.Context, id int, event Event) bool {
	ch, err := d.loadChannel(ctx, id)
	if err != nil {
		d.logFn("ERROR", "broadcast %s: loading channel %d: %v", event.Type, id, err)
		return false
	}
	sender, ok := d.senders[ch.Type]
	if !ok {
		msg := fmt.Sprintf("no sender for type %q", ch.Type)
		if err := d.logDelivery(ctx, ch.ID, event, "error", msg); err != nil {
			d.logFn("ERROR", "broadcast %s: %v", event.Type, err)
		}
		return false
	}
	sendErr := RedactError(sender.Send(ctx, *ch, event))
	status, errMsg := "sent", ""
	if sendErr != nil {
		status, errMsg = "error", sendErr.Error()
		d.logFn("ERROR", "broadcast %s to channel %d: %s", event.Type, id, errMsg)
	}
	if err := d.logDelivery(ctx, ch.ID, event, status, errMsg); err != nil {
		d.logFn("ERROR", "broadcast %s: %v", event.Type, err)
	}
	return sendErr == nil
}

// EnabledChannelIDs lists every enabled channel (security broadcasts).
func (s *PoolStore) EnabledChannelIDs(ctx context.Context) ([]int, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("list channels: notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx,
		`/* pg_sage notify_broadcast v1 */ SELECT id FROM sage.notification_channels
		 WHERE enabled = true ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan channel id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
