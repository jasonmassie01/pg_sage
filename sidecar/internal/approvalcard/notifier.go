package approvalcard

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// DefaultCardTTL bounds how long a chat card's buttons work; the queue
// item's own expiry bounds it further.
const DefaultCardTTL = 24 * time.Hour

// Dispatcher routes notification events (notify.Dispatcher).
type Dispatcher interface {
	Dispatch(ctx context.Context, evt notify.Event) error
}

// Notifier turns an approval request for a queued action into its card;
// every other event passes through unchanged.
type Notifier struct {
	inner  Dispatcher
	loader Loader
	logFn  func(string, string, ...any)
	// trust reads the database's current trust level (it can change at run
	// time); nil keeps the loader's.
	trust func() string
}

// NewNotifier wraps inner for one database; nil when there is no inner
// dispatcher (notifications off).
func NewNotifier(inner Dispatcher, l Loader) *Notifier {
	if inner == nil {
		return nil
	}
	return &Notifier{inner: inner, loader: l, logFn: func(string, string, ...any) {}}
}

// WithLog sets the logger of card failures.
func (n *Notifier) WithLog(logFn func(string, string, ...any)) *Notifier {
	if n != nil && logFn != nil {
		n.logFn = logFn
	}
	return n
}

// WithTrust reads the trust level for every card from trust.
func (n *Notifier) WithTrust(trust func() string) *Notifier {
	if n != nil {
		n.trust = trust
	}
	return n
}

// Dispatch sends evt, as a card when it asks approval for a queue item.
func (n *Notifier) Dispatch(ctx context.Context, evt notify.Event) error {
	if n == nil {
		return nil
	}
	if evt.Type == "approval_needed" {
		evt = n.enrich(ctx, evt)
	}
	return n.inner.Dispatch(ctx, evt)
}

// enrich attaches the card. A Sage SRE proposal keeps its own buttons; a
// card that cannot be built is logged and the plain request goes out.
func (n *Notifier) enrich(ctx context.Context, evt notify.Event) notify.Event {
	id, ok := evt.Data["queue_id"].(int)
	if !ok || id <= 0 {
		return evt
	}
	if _, sre := evt.Data["approval_proposal_id"]; sre {
		return evt
	}
	l := n.loader
	if n.trust != nil {
		l.TrustLevel = n.trust()
	}
	c, err := l.Card(ctx, id)
	if err != nil {
		n.logFn("approvalcard", "queue item %d sent without its card: %v", id, err)
		return evt
	}
	ref := notify.CardRef{Database: c.Database, QueueID: c.QueueID, CardHash: c.CardHash,
		Title: c.Title, Summary: Summary(c), ExpiresAt: c.ExpiresAt}
	return notify.WithApprovalCard(evt, ref, Text(c, n.loader.now()))
}

// RenotifySnoozed ends the snoozes that ran out and sends those cards
// again, once per snooze.
func RenotifySnoozed(ctx context.Context, l Loader, d Dispatcher) (int, error) {
	if l.Pool == nil || d == nil {
		return 0, fmt.Errorf("approvalcard: re-notify needs a pool and a dispatcher")
	}
	rows, err := l.Pool.Query(ctx, `/* pg_sage */ UPDATE sage.action_queue q
		SET snoozed_until = NULL
		WHERE q.snoozed_until IS NOT NULL AND q.snoozed_until <= now()
		  AND q.status = 'pending' AND q.expires_at > now()
		RETURNING q.id, q.proposed_sql, q.action_risk,
		  (SELECT COALESCE(f.title, '') FROM sage.findings f WHERE f.id = q.finding_id)`)
	if err != nil {
		return 0, fmt.Errorf("approvalcard: end snoozes: %w", err)
	}
	type ended struct {
		id               int
		sql, risk, title string
	}
	var items []ended
	for rows.Next() {
		var e ended
		var title *string
		if err := rows.Scan(&e.id, &e.sql, &e.risk, &title); err != nil {
			rows.Close()
			return 0, fmt.Errorf("approvalcard: end snoozes: %w", err)
		}
		if title != nil {
			e.title = *title
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("approvalcard: end snoozes: %w", err)
	}
	for _, e := range items {
		evt := notify.QueuedApprovalEvent(e.title, e.sql, l.Database, e.risk, e.id)
		if err := d.Dispatch(ctx, evt); err != nil {
			return len(items), fmt.Errorf("approvalcard: re-send card %d: %w", e.id, err)
		}
	}
	return len(items), nil
}

// TokenIssuer mints card tokens in the control database
// (notify.CardTokenIssuer).
type TokenIssuer struct {
	Store *chatops.CardStore
	TTL   time.Duration
}

// Issue records the card for a channel; its buttons work until the
// earlier of the TTL and the queue item's expiry.
func (t TokenIssuer) Issue(ctx context.Context, ch notify.Channel,
	ref notify.CardRef) (string, error) {
	ttl := t.TTL
	if ttl <= 0 {
		ttl = DefaultCardTTL
	}
	expires := time.Now().Add(ttl)
	if !ref.ExpiresAt.IsZero() && ref.ExpiresAt.Before(expires) {
		expires = ref.ExpiresAt
	}
	return t.Store.Issue(ctx, chatops.CardIssue{ChannelID: ch.ID, Database: ref.Database,
		QueueID: ref.QueueID, CardHash: ref.CardHash, Title: ref.Title,
		Summary: ref.Summary, ExpiresAt: expires})
}

// Revoke forgets a card whose message was not delivered.
func (t TokenIssuer) Revoke(ctx context.Context, token string) error {
	return t.Store.Revoke(ctx, token)
}
