package notify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// reminderScanLimit bounds the open alerts one drain re-sends.
const reminderScanLimit = 100

// attempt delivers one claimed entry once and records the outcome: sent,
// pending (a retryable failure with attempts left) or failed.
func (d *Dispatcher) attempt(ctx context.Context, e OutboxEntry, now time.Time) string {
	evt, ch, sender, err := d.entryTarget(ctx, e)
	if err != nil {
		return d.finish(ctx, e, now, err)
	}
	evt, revoke := d.withCardToken(ctx, *ch, evt)
	evt, revokeFact := d.withFactToken(ctx, *ch, evt)
	evt, revokePlan := d.withPlanToken(ctx, *ch, evt)
	messageID, sendErr := send(ctx, sender, *ch, evt)
	if sendErr != nil {
		revoke()
		revokeFact()
		revokePlan()
	}
	status := d.finish(ctx, e, now, sendErr)
	rec := Delivery{ChannelID: ch.ID, Event: evt, Status: "sent", ProviderMessageID: messageID}
	if sendErr != nil {
		rec.Status, rec.Error, rec.ProviderMessageID = "error", RedactError(sendErr).Error(), ""
		if status == "pending" {
			rec.Status = "retrying"
		}
	}
	_ = d.recordDelivery(ctx, rec)
	return status
}

func (d *Dispatcher) entryTarget(
	ctx context.Context, e OutboxEntry,
) (Event, *Channel, Sender, error) {
	evt, err := decodeEvent(e.Payload)
	if err != nil {
		return Event{}, nil, nil, fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	ch, err := d.loadChannel(ctx, e.ChannelID)
	if err != nil {
		return evt, nil, nil, fmt.Errorf("loading channel %d: %w", e.ChannelID, err)
	}
	if !ch.Enabled {
		return evt, nil, nil, fmt.Errorf("channel %d is disabled: %w", ch.ID, ErrPermanent)
	}
	sender, ok := d.senders[ch.Type]
	if !ok {
		return evt, nil, nil, fmt.Errorf("no sender for type %q: %w", ch.Type, ErrPermanent)
	}
	return evt, ch, sender, nil
}

// finish writes the outcome of one attempt that ended with err.
func (d *Dispatcher) finish(
	ctx context.Context, e OutboxEntry, now time.Time, err error,
) string {
	o := AttemptOutcome{Status: "sent", Attempts: e.Attempts + 1, NextAttemptAt: now}
	if err != nil {
		o.LastError = RedactError(err).Error()
		o.Status = "failed"
		if Retryable(err) && o.Attempts < outboxMaxAttempts {
			o.Status, o.NextAttemptAt = "pending", now.Add(retryDelay(err, o.Attempts))
		}
	}
	if ferr := d.outbox.FinishAttempt(ctx, e.ID, o); ferr != nil {
		d.logFn("ERROR", "notification outbox entry %d: record %s: %v", e.ID, o.Status, ferr)
	}
	if o.Status == "failed" {
		d.logFn("ERROR", "notification to channel %d (%s %s) gave up after %d attempt(s): %s",
			e.ChannelID, e.Event, e.DedupKey, o.Attempts, o.LastError)
	}
	return o.Status
}

// outboxMaxRetryAfter bounds how long a Retry-After hint can hold an
// entry back, so a destination cannot park an alert for a day.
const outboxMaxRetryAfter = time.Hour

// retryDelay is the wait before attempt n+1: the backoff, or the
// destination's Retry-After hint when it is longer (market-14).
func retryDelay(err error, n int) time.Duration {
	d := outboxBackoff(n)
	var se *HTTPStatusError
	if errors.As(err, &se) && se.RetryAfter > d {
		d = min(se.RetryAfter, outboxMaxRetryAfter)
	}
	return d
}

// queueReminders queues one reminder for each open alert whose repeat
// interval has passed, and returns how many it queued.
func (d *Dispatcher) queueReminders(ctx context.Context, now time.Time) (int, error) {
	policy := d.alertPolicy()
	cutoffs := policy.reminderCutoffs(now)
	if len(cutoffs) == 0 {
		return 0, nil
	}
	due, err := d.outbox.DueReminders(ctx, cutoffs, reminderScanLimit)
	if err != nil {
		return 0, fmt.Errorf("list due reminders: %w", err)
	}
	queued := 0
	for _, st := range due {
		ok, err := d.outbox.Schedule(ctx, st.Key, func(cur *NotifyState) ScheduleStep {
			return reminderStep(policy, cur, now)
		})
		if err != nil {
			d.logFn("ERROR", "reminder for %s on channel %d: %v", st.Key.DedupKey,
				st.Key.ChannelID, err)
			continue
		}
		if ok {
			queued++
		}
	}
	return queued, nil
}

// reminderStep re-checks the state under the schedule lock and queues the
// reminder when it is still due.
func reminderStep(p AlertPolicy, cur *NotifyState, now time.Time) ScheduleStep {
	dec := p.DecideReminder(cur, now)
	if dec.Suppressed {
		return ScheduleStep{}
	}
	evt, err := reminderEvent(*cur, now)
	if err != nil {
		return ScheduleStep{}
	}
	payload, err := encodeEvent(evt)
	if err != nil {
		return ScheduleStep{}
	}
	next := p.Apply(cur, evt, dec)
	next.Key = cur.Key
	return ScheduleStep{State: next, Entry: &OutboxEntry{Database: cur.Key.Database,
		DedupKey: cur.Key.DedupKey, ChannelID: cur.Key.ChannelID, Event: evt.Type,
		Payload: payload, NextAttemptAt: dec.At, Status: "pending"}}
}
