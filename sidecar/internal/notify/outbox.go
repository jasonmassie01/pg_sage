package notify

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// outboxMaxAttempts bounds the deliveries of one entry.
	outboxMaxAttempts = 6
	// outboxBatch is how many due entries one drain claims.
	outboxBatch = 200
	// outboxRetention keeps finished entries and closed states this long.
	outboxRetention = 7 * 24 * time.Hour
)

// outboxBackoff is the wait after the n-th failed attempt: 30s doubling,
// at most 15 minutes (the YAML stack's exponential backoff, alerting-06).
func outboxBackoff(n int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < n && d < 15*time.Minute; i++ {
		d *= 2
	}
	return min(d, 15*time.Minute)
}

// DispatchResult counts what Submit did with one event's matching rules.
type DispatchResult struct {
	Enqueued   int // queued in the outbox
	Delivered  int // sent at once (no outbox, or a card event)
	Failed     int // sent at once and failed
	Refused    int // could not be delivered or queued (no sender, store error)
	Suppressed int // the alert state decided not to send (cooldown, resolved)
}

// DrainResult counts one outbox pass.
type DrainResult struct {
	Sent, Retried, Failed, Reminders int
}

// UseOutbox makes Submit queue deliveries in s and DrainOutbox deliver
// them. Until SetAlertPolicy, the alerting defaults apply.
func (d *Dispatcher) UseOutbox(s OutboxStore) {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	d.outbox = s
	if d.policy == nil {
		p, _ := NewAlertPolicy(0, nil, "", "", "")
		d.policy = &p
	}
}

// SetAlertPolicy replaces the cooldown, quiet hours and repeat intervals;
// the next decision uses it, without a restart.
func (d *Dispatcher) SetAlertPolicy(p AlertPolicy) {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	d.policy = &p
}

func (d *Dispatcher) alertPolicy() AlertPolicy {
	d.policyMu.RLock()
	defer d.policyMu.RUnlock()
	return *d.policy
}

func (d *Dispatcher) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// enqueue queues evt for ch, through the alert state for finding alerts.
func (d *Dispatcher) enqueue(ctx context.Context, ch Channel, evt Event, res *DispatchResult) {
	now := d.clock()
	key := StateKey{Database: eventDatabase(evt), DedupKey: EventDedupKey(evt), ChannelID: ch.ID}
	payload, err := encodeEvent(evt)
	if err != nil {
		d.logFn("ERROR", "channel %d: %v", ch.ID, err)
		res.Refused++
		return
	}
	entry := OutboxEntry{Database: key.Database, DedupKey: key.DedupKey, ChannelID: ch.ID,
		Event: evt.Type, Payload: payload, NextAttemptAt: d.quietHold(ch, evt, now),
		Status: "pending"}
	if !Throttled(evt.Type) {
		if err := d.outbox.Enqueue(ctx, entry); err != nil {
			d.logFn("ERROR", "channel %d: queue %s: %v", ch.ID, evt.Type, err)
			res.Refused++
			return
		}
		res.Enqueued++
		return
	}
	var dec Decision
	policy := d.policyFor(ch)
	queued, err := d.outbox.Schedule(ctx, key, func(cur *NotifyState) ScheduleStep {
		dec = policy.Decide(cur, evt, now)
		return scheduleStep(policy, cur, evt, dec, key, entry)
	})
	switch {
	case err != nil:
		d.logFn("ERROR", "channel %d: schedule %s %s: %v", ch.ID, evt.Type, key.DedupKey, err)
		res.Refused++
	case queued:
		res.Enqueued++
	default:
		d.logFn("DEBUG", "channel %d: %s %s not sent: %s", ch.ID, evt.Type, key.DedupKey,
			dec.Reason)
		res.Suppressed++
	}
}

// scheduleStep turns a decision into the state and entry to write.
func scheduleStep(
	p AlertPolicy, cur *NotifyState, evt Event, dec Decision, key StateKey, entry OutboxEntry,
) ScheduleStep {
	step := ScheduleStep{CancelPending: dec.CancelPending}
	if next := p.Apply(cur, evt, dec); next != cur {
		next.Key = key
		if !evt.Resolve && !dec.Reminder {
			next.Payload = entry.Payload
		}
		step.State = next
	}
	if !dec.Suppressed {
		entry.NextAttemptAt = dec.At
		step.Entry = &entry
	}
	return step
}

// RunOutbox drains the outbox every interval until ctx ends, and purges
// finished entries hourly.
func (d *Dispatcher) RunOutbox(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	var purged time.Time
	for {
		if _, err := d.DrainOutbox(ctx); err != nil && ctx.Err() == nil {
			d.logFn("ERROR", "notification outbox: %v", err)
		}
		if now := d.clock(); now.Sub(purged) >= time.Hour && d.outbox != nil {
			if err := d.outbox.PurgeOutbox(ctx, now.Add(-outboxRetention)); err != nil &&
				ctx.Err() == nil {
				d.logFn("WARN", "notification outbox purge: %v", err)
			}
			purged = now
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DrainOutbox queues due reminders, then delivers every due entry: one
// goroutine per channel, so a slow channel does not hold up the others.
func (d *Dispatcher) DrainOutbox(ctx context.Context) (DrainResult, error) {
	var res DrainResult
	if d.outbox == nil {
		return res, nil
	}
	now := d.clock()
	n, err := d.queueReminders(ctx, now)
	res.Reminders = n
	if err != nil {
		return res, err
	}
	entries, err := d.outbox.ClaimDue(ctx, now, outboxBatch)
	if err != nil {
		return res, fmt.Errorf("claim due notifications: %w", err)
	}
	byChannel := map[int][]OutboxEntry{}
	for _, e := range entries {
		byChannel[e.ChannelID] = append(byChannel[e.ChannelID], e)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, list := range byChannel {
		wg.Add(1)
		go func(list []OutboxEntry) {
			defer wg.Done()
			for _, e := range list {
				status := d.attempt(ctx, e, now)
				mu.Lock()
				res.add(status)
				mu.Unlock()
			}
		}(list)
	}
	wg.Wait()
	return res, nil
}

func (r *DrainResult) add(status string) {
	switch status {
	case "sent":
		r.Sent++
	case "pending":
		r.Retried++
	default:
		r.Failed++
	}
}
