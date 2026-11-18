package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"
)

// Ask batching (ux-09a). The Ask events of one database reach a channel as
// one message per window (notification_policy.ask_batch_minutes): the
// first Ask opens the window and every Ask before it ends joins the batch.
// The batch is kept in the channel's alert state and decided under the
// outbox's Schedule lock, so two Asks at the same instant, even from two
// sidecars, make one message. A batch of one goes out as the Ask itself.
// An interactive chat channel (CardInteractive) is not batched: its card
// buttons decide the change there, so each card goes out at once.

// askBatchItems is how many Asks one message lists; the rest are counted.
const askBatchItems = 10

// askBatchKeyPrefix starts the dedup key of a database's Ask batch.
const askBatchKeyPrefix = "ask-batch:"

// SetAskBatch sets the Ask batching window; 0 sends each Ask alone.
func (d *Dispatcher) SetAskBatch(window time.Duration) {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	d.askWindow = max(window, 0)
}

func (d *Dispatcher) askBatchWindow() time.Duration {
	d.policyMu.RLock()
	defer d.policyMu.RUnlock()
	return d.askWindow
}

// batchesAsk reports whether rule's delivery of event to ch is batched.
func (d *Dispatcher) batchesAsk(rule Rule, ch Channel) bool {
	return rule.DeliveryClass == string(ClassAsk) && d.outbox != nil &&
		d.askBatchWindow() > 0 && !CardInteractive(ch)
}

// askBatch is a database's open batch on one channel.
type askBatch struct {
	Start time.Time     `json:"start"`
	Count int           `json:"count"`
	Items []storedEvent `json:"items"`
}

// enqueueAsk adds evt to its database's batch for ch and requeues the
// batch's one message for the end of its window.
func (d *Dispatcher) enqueueAsk(ctx context.Context, ch Channel, evt Event,
	res *DispatchResult) {
	db := eventDatabase(evt)
	key := StateKey{Database: db, DedupKey: askBatchKeyPrefix + db, ChannelID: ch.ID}
	now, window := d.clock(), d.askBatchWindow()
	var stepErr error
	queued, err := d.outbox.Schedule(ctx, key, func(cur *NotifyState) ScheduleStep {
		step, err := askBatchStep(cur, key, plainEvent(evt), now, window)
		stepErr = err
		return step
	})
	if err == nil {
		err = stepErr
	}
	switch {
	case err != nil:
		d.logFn("ERROR", "channel %d: batch %s for %q: %v", ch.ID, evt.Type, db, err)
		res.Refused++
	case queued:
		res.Enqueued++
	}
}

// askBatchStep is the state and message of the batch after evt joins it:
// the current batch while its window is open (its pending message is
// replaced), a new one otherwise (the ended batch's message still goes).
func askBatchStep(cur *NotifyState, key StateKey, evt Event, now time.Time,
	window time.Duration) (ScheduleStep, error) {
	var b askBatch
	if cur != nil && len(cur.Payload) > 0 {
		if err := json.Unmarshal(cur.Payload, &b); err != nil {
			b = askBatch{}
		}
	}
	open := b.Count > 0 && now.Before(b.Start.Add(window))
	if !open {
		b = askBatch{Start: now}
	}
	b.Count++
	if len(b.Items) < askBatchItems {
		b.Items = append(b.Items, storedEvent{Type: evt.Type, Severity: evt.Severity,
			Subject: evt.Subject, Body: evt.Body, Data: evt.Data, DedupKey: evt.DedupKey})
	}
	state, err := json.Marshal(b)
	if err != nil {
		return ScheduleStep{}, fmt.Errorf("encode ask batch: %w", err)
	}
	payload, err := encodeEvent(b.message(key.Database, key.DedupKey))
	if err != nil {
		return ScheduleStep{}, err
	}
	entry := OutboxEntry{Database: key.Database, DedupKey: key.DedupKey,
		ChannelID: key.ChannelID, Event: "approval_needed", Payload: payload,
		NextAttemptAt: b.Start.Add(window), Status: "pending"}
	return ScheduleStep{CancelPending: open, Entry: &entry,
		State: &NotifyState{Key: key, Severity: evt.Severity, Payload: state}}, nil
}

// plainEvent is evt without the typed card values the outbox cannot keep.
func plainEvent(evt Event) Event {
	out := evt
	out.Data = maps.Clone(evt.Data)
	for _, k := range syncOnlyData {
		delete(out.Data, k)
	}
	return out
}

// message is the batch's one message: a lone Ask as itself, otherwise its
// first items, the count of the rest and where to decide them.
func (b askBatch) message(db, dedupKey string) Event {
	if b.Count == 1 && len(b.Items) == 1 {
		it := b.Items[0]
		return Event{Type: it.Type, Severity: it.Severity, Subject: it.Subject,
			Body: it.Body, Data: it.Data, DedupKey: it.DedupKey}
	}
	lines := make([]string, 0, len(b.Items)+3)
	for _, it := range b.Items {
		lines = append(lines, "- "+it.Subject)
	}
	if more := b.Count - len(b.Items); more > 0 {
		lines = append(lines, fmt.Sprintf("%d more in pg_sage", more))
	}
	lines = append(lines, "", "Open them in pg_sage: #/inbox?database="+url.QueryEscape(db),
		"Approve, reject or snooze each in pg_sage, or mute this class on the "+
			"Notifications page.")
	return Event{Type: "approval_needed", Severity: "warning",
		Subject: fmt.Sprintf("%d decisions waiting on %s", b.Count, db),
		Body:    strings.Join(lines, "\n"), DedupKey: dedupKey,
		Data: map[string]any{"database": db, "ask_batch": b.Count}}
}
