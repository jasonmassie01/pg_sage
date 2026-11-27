package briefing

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Sending the digest (ux-02b): one digest_ready event through
// Notifications, so every channel kind gets it under the owner's rules,
// plus the local stdout channel. "Only when changed" (the default) stores
// an unchanged or empty digest without sending it; the check is one
// INSERT ... ON CONFLICT on sage.digest_delivery, so two leaders that both
// produce the same digest send it once.

// DeliverySuppressed is a digest stored but not sent: empty, or equal to
// the last one delivered for its scope.
const DeliverySuppressed = "suppressed_unchanged"

// Delivery-status keys that are not a channel name.
const (
	deliveryKeyDigest        = "digest"
	deliveryKeyNotifications = "notifications"
)

// Notifier sends the digest through Notifications: the dispatcher's
// SubmitWithOutcomes and its store's EnsureDefaultRule.
type Notifier interface {
	SubmitWithOutcomes(ctx context.Context, e notify.Event) (
		notify.DispatchResult, []notify.ChannelOutcome, error)
	EnsureDefaultRule(ctx context.Context, event string) (bool, error)
}

// WithNotifier sends the worker's digests through n.
func (w *Worker) WithNotifier(n Notifier) *Worker {
	w.notifier = n
	return w
}

// digestSend is one digest message: a database's, or the fleet roll-up's.
type digestSend struct {
	scope      string // db:<name> or fleet
	database   string // the event's database; "all" for the roll-up
	hash       string
	empty      bool
	briefingID int64
	headline   string
	text       string
}

// digestScope is the scope a database's digest is deduplicated in.
func (w *Worker) digestScope() string {
	if w.database == "" {
		return "db:default"
	}
	return "db:" + w.database
}

const claimDigestSQL = `/* pg_sage */
INSERT INTO sage.digest_delivery AS d (scope, digest_hash, briefing_id, delivered_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (scope) DO UPDATE SET digest_hash = EXCLUDED.digest_hash,
    briefing_id = EXCLUDED.briefing_id, delivered_at = now()
WHERE d.digest_hash IS DISTINCT FROM EXCLUDED.digest_hash
RETURNING scope`

// claimDigest records hash as delivered for scope and reports whether it
// differs from the last one (true: send it).
func (w *Worker) claimDigest(ctx context.Context, s digestSend) (bool, error) {
	var id any
	if s.briefingID > 0 {
		id = s.briefingID
	}
	tag, err := w.pool.Exec(ctx, claimDigestSQL, s.scope, s.hash, id)
	if err != nil {
		return false, fmt.Errorf("claim digest for %s: %w", s.scope, err)
	}
	return tag.RowsAffected() == 1, nil
}

// unclaimDigest forgets a claim that reached no channel, so the same
// digest is tried again at the next schedule.
func (w *Worker) unclaimDigest(ctx context.Context, s digestSend) {
	if _, err := w.pool.Exec(ctx, `/* pg_sage */ DELETE FROM sage.digest_delivery
		WHERE scope = $1 AND digest_hash = $2`, s.scope, s.hash); err != nil {
		w.logFn("WARN", "briefing: forget undelivered digest for %s: %v", s.scope, err)
	}
}

// deliverDigest sends s unless "only when changed" holds it back, and
// returns each channel's outcome.
func (w *Worker) deliverDigest(ctx context.Context, s digestSend) map[string]Delivery {
	only := w.settings().cfg.OnlyWhenChanged
	if only && s.empty {
		return map[string]Delivery{deliveryKeyDigest: delivered(DeliverySuppressed,
			"nothing to report")}
	}
	claimed := false
	if w.pool != nil {
		fresh, err := w.claimDigest(ctx, s)
		if err != nil {
			// Sending a digest twice is harmless; losing one is not.
			w.logFn("WARN", "briefing: %v; sending the digest unchecked", err)
		}
		if only && err == nil && !fresh {
			return map[string]Delivery{deliveryKeyDigest: delivered(DeliverySuppressed,
				"unchanged since the last digest")}
		}
		claimed = err == nil
	}
	out := w.Dispatch(ctx, s.text)
	missed := w.notifyDigest(ctx, s, out)
	// A digest the Notifications channels all missed is tried again at the
	// next schedule even when stdout took it: stdout alone must not mark it
	// delivered, or an unchanged digest would never reach a person.
	if claimed && (missed || !reachedAChannel(out)) {
		w.unclaimDigest(ctx, s)
	}
	return out
}

// notifyDigest submits the digest_ready event and adds each channel's
// outcome to out, keyed by channel name. It reports whether Notifications
// had somewhere to send the digest and no channel took it (missed).
func (w *Worker) notifyDigest(
	ctx context.Context, s digestSend, out map[string]Delivery,
) (missed bool) {
	if w.notifier == nil {
		out[deliveryKeyNotifications] = delivered(DeliverySkipped,
			"no notification dispatcher is wired")
		return false
	}
	if created, err := w.notifier.EnsureDefaultRule(ctx, notify.EventDigestReady); err != nil {
		w.logFn("WARN", "briefing: default digest_ready rule: %v", err)
	} else if created {
		w.logFn("INFO", "briefing: added a digest_ready rule on the first notification "+
			"channel that suits a digest; disable it to stop digests there")
	}
	_, outs, err := w.notifier.SubmitWithOutcomes(ctx, w.digestEventFor(ctx, s))
	if err != nil {
		w.logFn("WARN", "briefing: send digest through notifications: %v", err)
		out[deliveryKeyNotifications] = delivered(DeliveryError,
			"notifications: "+notify.RedactError(err).Error())
		return true
	}
	if len(outs) == 0 {
		out[deliveryKeyNotifications] = delivered(DeliverySkipped,
			"no enabled digest_ready notification rule")
		return false
	}
	now, missed := time.Now().UTC(), true
	for _, o := range outs {
		status := o.Status
		if status == notify.OutcomeRefused {
			status = DeliveryError
		}
		out[o.Channel] = Delivery{Status: status, Error: o.Error, At: now}
		missed = missed && !tookDigest(status)
	}
	return missed
}

// tookDigest reports whether a channel status means the digest got there.
func tookDigest(status string) bool {
	return status == DeliverySent || status == notify.OutcomeQueued
}

// reachedAChannel reports whether any channel took the digest.
func reachedAChannel(out map[string]Delivery) bool {
	for _, d := range out {
		if tookDigest(d.Status) {
			return true
		}
	}
	return false
}

// digestEvent is the digest_ready event for s.
func digestEvent(s digestSend) notify.Event {
	return notify.Event{Type: notify.EventDigestReady, Severity: "info",
		Subject: "pg_sage digest: " + s.headline, Body: s.text,
		Data: map[string]any{"database": s.database, "digest_hash": s.hash,
			"scope": s.scope},
		DedupKey: "digest:" + s.scope + ":" + s.hash}
}

// WeeklyLineFunc returns the weekly notification line, "N messages, M led
// to an action" over the last 7 days (ux-09b).
type WeeklyLineFunc func(ctx context.Context) (string, error)

// WithWeeklyLine adds fn's line to every digest.
func (w *Worker) WithWeeklyLine(fn WeeklyLineFunc) *Worker {
	w.weeklyLine = fn
	return w
}

// digestEventFor is s's digest event with the weekly line appended; a
// failed read is logged and the digest goes without it.
func (w *Worker) digestEventFor(ctx context.Context, s digestSend) notify.Event {
	evt := digestEvent(s)
	if w.weeklyLine == nil {
		return evt
	}
	line, err := w.weeklyLine(ctx)
	if err != nil {
		w.logFn("WARN", "briefing: digest sent without the weekly line: %v", err)
		return evt
	}
	evt.Body += "\n\nLast 7 days: " + line
	return evt
}
