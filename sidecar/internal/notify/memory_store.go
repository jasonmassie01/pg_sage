package notify

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store for tests: rules, channels, the
// delivery log, the outbox and the alert state behind one mutex, so
// Schedule is atomic like the pool store's advisory lock. The Fail*
// fields make the matching call return that error.
type MemoryStore struct {
	mu         sync.Mutex
	channels   map[int]Channel
	rules      []Rule
	deliveries []Delivery
	outbox     []OutboxEntry
	states     map[StateKey]NotifyState
	nextID     int64
	channelSeq int
	ruleSeq    int
	hashes     map[string]string // "c<id>" / "r<id>" -> config_hash

	FailRules, FailSchedule, FailClaim error
	// FailReconcile fails a reconcile write of channel FailReconcileAt.
	FailReconcile   error
	FailReconcileAt string
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{channels: map[int]Channel{}, states: map[StateKey]NotifyState{},
		hashes: map[string]string{}}
}

var _ Store = (*MemoryStore)(nil)

// AddChannel stores ch under the next id (ch.ID when set) and returns it.
func (m *MemoryStore) AddChannel(ch Channel) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ch.ID == 0 {
		ch.ID = m.channelSeq + 1
	}
	if ch.ID > m.channelSeq {
		m.channelSeq = ch.ID
	}
	m.channels[ch.ID] = ch
	return ch.ID
}

// AddRule stores r with the next id.
func (m *MemoryStore) AddRule(r Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ruleSeq++
	r.ID = m.ruleSeq
	m.rules = append(m.rules, r)
}

// MatchingRules returns the enabled rules of eventType.
func (m *MemoryStore) MatchingRules(_ context.Context, eventType string) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailRules != nil {
		return nil, fmt.Errorf("query rules: %w", m.FailRules)
	}
	var out []Rule
	for _, r := range m.rules {
		if r.Event == eventType && r.Enabled {
			out = append(out, r)
		}
	}
	return out, nil
}

// Channel returns a copy of channel id.
func (m *MemoryStore) Channel(_ context.Context, id int) (*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[id]
	if !ok {
		return nil, fmt.Errorf("get channel %d: not found", id)
	}
	ch.Config = copyConfig(ch.Config)
	return &ch, nil
}

// LogDelivery records one attempt.
func (m *MemoryStore) LogDelivery(
	ctx context.Context, channelID int, event Event, status, errMsg string,
) error {
	return m.RecordDelivery(ctx, Delivery{ChannelID: channelID, Event: event,
		Status: status, Error: errMsg})
}

// RecordDelivery records one attempt.
func (m *MemoryStore) RecordDelivery(_ context.Context, d Delivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries = append(m.deliveries, d)
	return nil
}

// Deliveries returns the recorded attempts in order.
func (m *MemoryStore) Deliveries() []Delivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Delivery(nil), m.deliveries...)
}

// Outbox returns the outbox entries in insertion order.
func (m *MemoryStore) Outbox() []OutboxEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]OutboxEntry(nil), m.outbox...)
}

// States returns the alert states, ordered by database then dedup key.
func (m *MemoryStore) States() []NotifyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NotifyState, 0, len(m.states))
	for _, st := range m.states {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Database != out[j].Key.Database {
			return out[i].Key.Database < out[j].Key.Database
		}
		return out[i].Key.DedupKey < out[j].Key.DedupKey
	})
	return out
}

// Schedule runs fn under the store's lock and applies its step.
func (m *MemoryStore) Schedule(
	_ context.Context, key StateKey, fn func(cur *NotifyState) ScheduleStep,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSchedule != nil {
		return false, m.FailSchedule
	}
	var cur *NotifyState
	if st, ok := m.states[key]; ok {
		cur = &st
	}
	step := fn(cur)
	if step.CancelPending {
		for i, e := range m.outbox {
			if e.Status == "pending" && e.ChannelID == key.ChannelID &&
				e.Database == key.Database && e.DedupKey == key.DedupKey {
				m.outbox[i].Status = "cancelled"
			}
		}
	}
	if step.State != nil {
		m.states[key] = *step.State
	}
	if step.Entry != nil {
		m.appendEntry(*step.Entry)
	}
	return step.Entry != nil, nil
}

// Enqueue queues e.
func (m *MemoryStore) Enqueue(_ context.Context, e OutboxEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailSchedule != nil {
		return m.FailSchedule
	}
	m.appendEntry(e)
	return nil
}

func (m *MemoryStore) appendEntry(e OutboxEntry) {
	m.nextID++
	e.ID = m.nextID
	e.Status = "pending"
	m.outbox = append(m.outbox, e)
}

// ClaimDue marks up to limit due pending entries as sending.
func (m *MemoryStore) ClaimDue(_ context.Context, now time.Time, limit int) ([]OutboxEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailClaim != nil {
		return nil, m.FailClaim
	}
	var out []OutboxEntry
	for i, e := range m.outbox {
		if len(out) >= limit {
			break
		}
		if e.Status == "pending" && !e.NextAttemptAt.After(now) {
			m.outbox[i].Status = "sending"
			out = append(out, m.outbox[i])
		}
	}
	return out, nil
}

// FinishAttempt records an attempt's outcome on entry id.
func (m *MemoryStore) FinishAttempt(_ context.Context, id int64, o AttemptOutcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].ID == id {
			e := &m.outbox[i]
			e.Status, e.Attempts, e.NextAttemptAt, e.LastError =
				o.Status, o.Attempts, o.NextAttemptAt, o.LastError
			return nil
		}
	}
	return fmt.Errorf("outbox entry %d not found", id)
}

// DueReminders lists, oldest first, the open states past their
// severity's cutoff, like the pool store.
func (m *MemoryStore) DueReminders(
	_ context.Context, cutoffs map[string]time.Time, limit int,
) ([]NotifyState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []NotifyState
	for _, st := range m.states {
		cutoff, ok := cutoffs[st.Severity]
		if st.Open && ok && !st.LastSentAt.After(cutoff) {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSentAt.Before(out[j].LastSentAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeOutbox drops finished entries and closed states older than before.
func (m *MemoryStore) PurgeOutbox(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.outbox[:0]
	for _, e := range m.outbox {
		if e.Status == "pending" || e.Status == "sending" || e.NextAttemptAt.After(before) {
			kept = append(kept, e)
		}
	}
	m.outbox = kept
	for k, st := range m.states {
		if !st.Open && st.LastSentAt.Before(before) && st.ResolvedAt.Before(before) {
			delete(m.states, k)
		}
	}
	return nil
}
