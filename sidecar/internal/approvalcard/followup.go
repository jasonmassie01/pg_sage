package approvalcard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Follow-up defaults.
const (
	// DefaultFollowupGiveUp closes a card whose action never reached a
	// verdict (verification windows are hours, not weeks).
	DefaultFollowupGiveUp = 14 * 24 * time.Hour
	defaultFollowupBatch  = 500
	// A card that stays open is checked again after an eighth of its age,
	// between these bounds.
	minFollowupDelay = time.Minute
	maxFollowupDelay = time.Hour
)

// DeliveryStore lists cards waiting for their follow-up and claims one
// before it is posted.
type DeliveryStore interface {
	PendingFollowups(ctx context.Context, limit int) ([]chatops.CardDelivery, error)
	ClaimFollowup(ctx context.Context, id int64, fence chatops.ClaimFence) (bool, error)
	MarkFollowedUp(ctx context.Context, id int64, verdict string) error
	DeferFollowup(ctx context.Context, id int64, delay time.Duration) error
}

// followupDelay is how long a card of this age waits for its next check:
// an eighth of its age, at least a minute and at most an hour.
func followupDelay(age time.Duration) time.Duration {
	return min(maxFollowupDelay, max(minFollowupDelay, age/8))
}

// ChannelSender posts to one channel by id.
type ChannelSender interface {
	SendToChannel(ctx context.Context, channelID int, evt notify.Event) error
}

// Followups posts each card's verification verdict to the chat the card
// went to, once: as a reply to the decision's message when the decision
// was made there (Telegram), otherwise to the channel.
type Followups struct {
	Store  DeliveryStore
	Sender ChannelSender
	// Pools resolves a monitored database's pool (nil: removed).
	Pools  func(database string) *pgxpool.Pool
	GiveUp time.Duration
	Batch  int
	Now    func() time.Time
	// Fence is the leader lease every post is claimed under, read again
	// before each post; ok false (no longer the leader) skips the post.
	// nil claims unfenced (leader election disabled).
	Fence func() (chatops.ClaimFence, bool)
}

// RunOnce follows up every due card whose outcome is final. It returns how
// many follow-ups were posted. A card that stays open (no verdict yet, or
// its outcome or post failed) is checked again after followupDelay, so it
// does not keep newer cards out of the batch.
func (f *Followups) RunOnce(ctx context.Context) (int, error) {
	if f == nil || f.Store == nil || f.Sender == nil || f.Pools == nil {
		return 0, errors.New("approvalcard: follow-ups are not configured")
	}
	batch := f.Batch
	if batch <= 0 {
		batch = defaultFollowupBatch
	}
	items, err := f.Store.PendingFollowups(ctx, batch)
	if err != nil {
		return 0, fmt.Errorf("approvalcard: list follow-ups: %w", err)
	}
	var errs []error
	sent := 0
	for _, d := range items {
		posted, err := f.followUp(ctx, d)
		if err != nil {
			errs = append(errs, err)
		}
		if posted {
			sent++
		}
	}
	return sent, errors.Join(errs...)
}

func (f *Followups) followUp(ctx context.Context, d chatops.CardDelivery) (bool, error) {
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	giveUp := f.GiveUp
	if giveUp <= 0 {
		giveUp = DefaultFollowupGiveUp
	}
	if now.Sub(d.CreatedAt) > giveUp {
		return false, f.close(ctx, d, "timed_out")
	}
	pool := f.Pools(d.Database)
	if pool == nil {
		return false, f.close(ctx, d, "database_removed")
	}
	o, err := ReadOutcome(ctx, pool, d.QueueID)
	if errors.Is(err, ErrNotFound) {
		return false, f.close(ctx, d, "queue_item_missing")
	}
	if err != nil {
		return false, errors.Join(fmt.Errorf(
			"approvalcard: outcome of queue item %d: %w", d.QueueID, err), f.later(ctx, d, now))
	}
	if !o.Final {
		return false, f.later(ctx, d, now)
	}
	if claimed, err := f.claim(ctx, d); err != nil || !claimed {
		return false, err
	}
	evt := notify.ApprovalOutcomeEvent(notify.ApprovalOutcome{Database: d.Database,
		QueueID: d.QueueID, Title: d.Title, Verdict: o.Verdict, Detail: o.Detail,
		Predicted: d.Summary, ReplyTo: d.MessageID})
	err = f.Sender.SendToChannel(ctx, d.ChannelID, evt)
	if errors.Is(err, notify.ErrChannelDisabled) {
		return false, f.close(ctx, d, "channel_disabled")
	}
	if err != nil {
		return false, errors.Join(fmt.Errorf(
			"approvalcard: follow-up of queue item %d to channel %d: %w",
			d.QueueID, d.ChannelID, err), f.later(ctx, d, now))
	}
	return true, f.close(ctx, d, o.Verdict)
}

// later pushes an open card's next check back by followupDelay. A card
// closed meanwhile needs no check.
func (f *Followups) later(ctx context.Context, d chatops.CardDelivery, now time.Time) error {
	err := f.Store.DeferFollowup(ctx, d.ID, followupDelay(now.Sub(d.CreatedAt)))
	if err != nil && !errors.Is(err, chatops.ErrCardUnknown) {
		return fmt.Errorf("approvalcard: defer follow-up %d: %w", d.ID, err)
	}
	return nil
}

// claim takes d's follow-up for this leader's epoch, right before the
// post; false means another leader has it or this one lost the lease.
func (f *Followups) claim(ctx context.Context, d chatops.CardDelivery) (bool, error) {
	fence := chatops.ClaimFence{}
	if f.Fence != nil {
		var ok bool
		if fence, ok = f.Fence(); !ok {
			return false, nil
		}
	}
	claimed, err := f.Store.ClaimFollowup(ctx, d.ID, fence)
	if err != nil {
		return false, fmt.Errorf("approvalcard: claim follow-up %d (queue item %d): %w",
			d.ID, d.QueueID, err)
	}
	return claimed, nil
}

func (f *Followups) close(ctx context.Context, d chatops.CardDelivery, verdict string) error {
	if err := f.Store.MarkFollowedUp(ctx, d.ID, verdict); err != nil &&
		!errors.Is(err, chatops.ErrCardUnknown) {
		return fmt.Errorf("approvalcard: close follow-up %d: %w", d.ID, err)
	}
	return nil
}
