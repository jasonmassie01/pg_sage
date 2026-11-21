package approvalcard

import (
	"context"
	"errors"
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
	// chatCap is notification_policy.chat_approval_max_tier ("" is the
	// default, moderate): a card above it is sent without Approve.
	chatCap string
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

// WithChatCap sets the chat approval cap the cards are sent under.
func (n *Notifier) WithChatCap(cap string) *Notifier {
	if n != nil {
		n.chatCap = cap
	}
	return n
}

// Dispatch sends evt, as a card when it asks approval for a queue item.
func (n *Notifier) Dispatch(ctx context.Context, evt notify.Event) error {
	if n == nil {
		return nil
	}
	if evt.Type == "approval_needed" {
		var skip bool
		if evt, skip = n.enrich(ctx, evt); skip {
			return nil
		}
	}
	return n.inner.Dispatch(ctx, evt)
}

// enrich attaches the card; a card that cannot be built is logged and the
// plain request goes out (without buttons: it is decided in the UI). A
// plan step's request is not sent at all (skip): it is decided on its plan.
func (n *Notifier) enrich(ctx context.Context, evt notify.Event) (notify.Event, bool) {
	id, ok := evt.Data["queue_id"].(int)
	if !ok || id <= 0 {
		return evt, false
	}
	l := n.loader
	if n.trust != nil {
		l.TrustLevel = n.trust()
	}
	c, err := l.Card(ctx, id)
	if errors.Is(err, ErrPlanStepItem) {
		n.logFn("approvalcard", "queue item %d is a plan step: no approval request sent", id)
		return evt, true
	}
	if err != nil {
		n.logFn("approvalcard", "queue item %d sent without its card: %v", id, err)
		return evt, false
	}
	ref, text := CardRefOfCapped(c, n.chatCap), Text(c, n.loader.now())
	if ref.NoApprove {
		text += "\n\n" + ref.ApproveIn
	}
	return notify.WithApprovalCard(evt, ref, text), false
}

// TokenIssuer mints card tokens in the control database
// (notify.CardTokenIssuer).
type TokenIssuer struct {
	Store *chatops.CardStore
	TTL   time.Duration
}

// Issue records the card for a channel; its buttons work until the
// earliest of the TTL, the card's own MaxTTL and the queue item's expiry.
func (t TokenIssuer) Issue(ctx context.Context, ch notify.Channel,
	ref notify.CardRef) (string, error) {
	ttl := t.TTL
	if ttl <= 0 {
		ttl = DefaultCardTTL
	}
	if ref.MaxTTL > 0 && ref.MaxTTL < ttl {
		ttl = ref.MaxTTL
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
