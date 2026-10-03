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
)

// DeliveryStore lists cards waiting for their follow-up.
type DeliveryStore interface {
	PendingFollowups(ctx context.Context, limit int) ([]chatops.CardDelivery, error)
	MarkFollowedUp(ctx context.Context, id int64, verdict string) error
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
}

// RunOnce follows up every card whose outcome is final. It returns how
// many follow-ups were posted; failed posts are retried on the next run.
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
	if err != nil || !o.Final {
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
		return false, fmt.Errorf("approvalcard: follow-up of queue item %d to channel %d: %w",
			d.QueueID, d.ChannelID, err)
	}
	return true, f.close(ctx, d, o.Verdict)
}

func (f *Followups) close(ctx context.Context, d chatops.CardDelivery, verdict string) error {
	if err := f.Store.MarkFollowedUp(ctx, d.ID, verdict); err != nil &&
		!errors.Is(err, chatops.ErrCardUnknown) {
		return fmt.Errorf("approvalcard: close follow-up %d: %w", d.ID, err)
	}
	return nil
}
