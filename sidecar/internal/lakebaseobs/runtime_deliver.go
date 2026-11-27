package lakebaseobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ackCommitter is the part of Store that acknowledges events inside the
// batch commit that stores them.
type ackCommitter interface {
	CommitBatchAcked(context.Context, string, Stream, Cursor, Cursor, []Event, time.Duration,
		[]string) ([]Event, error)
}

// subscriptions resolves each StreamConsumer's streams and, per runtime
// stream, the consumers that do not subscribe to it.
func subscriptions(streams []Stream, consumers []Consumer,
) (map[string][]Stream, map[Stream][]Consumer, error) {
	subscribed := map[string][]Stream{}
	atCommit := map[Stream][]Consumer{}
	for _, c := range consumers {
		sc, ok := c.(StreamConsumer)
		if !ok || len(sc.Streams()) == 0 {
			continue
		}
		own := slices.Clone(sc.Streams())
		for _, s := range own {
			if _, known := specs[s]; !known {
				return nil, nil, fmt.Errorf(
					"lakebase telemetry consumer %s subscribes to unknown stream %q", c.Name(), s)
			}
		}
		subscribed[c.Name()] = own
		for _, s := range streams {
			if !slices.Contains(own, s) {
				atCommit[s] = append(atCommit[s], c)
			}
		}
	}
	return subscribed, atCommit, nil
}

// commit stores a batch and advances the cursor. Consumers that do not
// subscribe to the stream are acknowledged in the same transaction, and each
// sees the newly stored events through Observe once the commit succeeded.
func (r *Runtime) commit(ctx context.Context, cs cursorStore, sp *streamPoll, after Cursor,
	events []Event,
) error {
	consumers := r.atCommit[sp.stream]
	if len(consumers) == 0 {
		return cs.CommitBatch(ctx, r.binding, sp.stream, sp.cursor, after, events,
			r.options.Retention)
	}
	ac, ok := cs.(ackCommitter)
	if !ok {
		return errAckUnsupported
	}
	names := make([]string, len(consumers))
	for i, c := range consumers {
		names[i] = c.Name()
	}
	inserted, err := ac.CommitBatchAcked(ctx, r.binding, sp.stream, sp.cursor, after, events,
		r.options.Retention, names)
	if err != nil {
		return err
	}
	for _, c := range consumers {
		if o, ok := c.(OutcomeObserver); ok {
			for _, e := range inserted {
				o.Observe(e)
			}
		}
	}
	return nil
}

func (r *Runtime) deliver(ctx context.Context, now time.Time) []error {
	var errs []error
	for _, c := range r.consumers {
		if err := r.deliverTo(ctx, c, now); err != nil {
			errs = append(errs, err)
		}
		if err := r.measureBacklog(ctx, c, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// deliverTo offers up to MaxRows pending events to one consumer. A failed
// event is scheduled for a retry and the next event is offered; policy
// revocation, cancellation, an unavailable sink or maxFailureStreak
// consecutive failures stop this consumer's poll.
// Consumer error text is never returned: it can carry SQL or payload values.
func (r *Runtime) deliverTo(ctx context.Context, c Consumer, now time.Time) error {
	events, err := r.store.PendingStreams(ctx, r.binding, c.Name(), r.subscribed[c.Name()],
		r.options.MaxRows, now)
	if err != nil {
		return err
	}
	failed, streak := 0, 0
	for _, e := range events {
		err = r.deliverOne(ctx, c, e, now)
		switch {
		case err == nil:
			streak = 0
			continue
		case errors.Is(err, ErrPolicy), errors.Is(err, ErrStalePolicy):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case sinkUnavailable(err):
			return fmt.Errorf("lakebase telemetry consumer %s sink unavailable (%s); "+
				"delivery resumes next poll", c.Name(), failureClass(err))
		}
		if err = r.retryLater(ctx, c, e, err, now); err != nil {
			return err
		}
		failed++
		if streak++; streak == maxFailureStreak {
			return fmt.Errorf("lakebase telemetry consumer %s failed %d consecutive events; "+
				"delivery resumes next poll", c.Name(), streak)
		}
	}
	if failed > 0 {
		return fmt.Errorf("lakebase telemetry consumer %s failed %d events; "+
			"each is retried up to %d attempts", c.Name(), failed, MaxDeliveryAttempts)
	}
	return nil
}

func (r *Runtime) deliverOne(ctx context.Context, c Consumer, e Event, now time.Time) error {
	deliveryCtx, cancel := context.WithTimeout(ctx, r.options.QueryTimeout)
	defer cancel()
	return r.store.DeliverAndAck(deliveryCtx, r.binding, e.ID, c, now)
}

// retryLater records a failed attempt. The attempt that reaches the cap is
// the one that acknowledged the event, so DeliveryFailed is reported once.
func (r *Runtime) retryLater(
	ctx context.Context, c Consumer, e Event, cause error, now time.Time,
) error {
	n, err := r.store.RecordDeliveryFailure(ctx, r.binding, e.ID, c.Name(), failureClass(cause),
		now)
	if err != nil {
		return fmt.Errorf("record lakebase telemetry delivery attempt for consumer %s: %w",
			c.Name(), err)
	}
	if o, ok := c.(OutcomeObserver); ok && n == MaxDeliveryAttempts {
		o.DeliveryFailed(e)
	}
	return nil
}

func (r *Runtime) measureBacklog(ctx context.Context, c Consumer, now time.Time) error {
	b, err := r.store.Backlog(ctx, r.binding, c.Name(), r.subscribed[c.Name()], now)
	if err != nil {
		return fmt.Errorf("measure lakebase telemetry backlog for consumer %s: %w", c.Name(), err)
	}
	status := DeliveryStatus{Consumer: c.Name(), PendingRows: b.Rows, PendingRowsCapped: b.Capped}
	if !b.Oldest.IsZero() {
		at := b.Oldest
		age := int64(now.Sub(at) / time.Second)
		status.OldestPendingAt, status.OldestPendingAgeSeconds = &at, &age
	}
	r.mu.Lock()
	r.deliveries[c.Name()] = status
	r.mu.Unlock()
	return nil
}

// Deliveries reports each consumer's backlog as of the last poll that
// measured it; a consumer is absent until then.
func (r *Runtime) Deliveries() []DeliveryStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeliveryStatus, 0, len(r.consumers))
	for _, c := range r.consumers {
		if d, ok := r.deliveries[c.Name()]; ok {
			out = append(out, d)
		}
	}
	return out
}
