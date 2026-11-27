package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// OutboxEntry is one queued delivery (sage.notification_outbox).
type OutboxEntry struct {
	ID            int64
	Database      string
	DedupKey      string
	ChannelID     int
	Event         string
	Payload       []byte
	Attempts      int
	NextAttemptAt time.Time
	// Status is pending, sending (claimed), sent, failed or cancelled.
	Status    string
	LastError string
}

// AttemptOutcome is what one delivery attempt leaves on its entry.
type AttemptOutcome struct {
	Status        string // sent, pending (retry) or failed
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
}

// ScheduleStep is what a schedule decision writes, atomically: the
// channel's next state (nil leaves it), the entry to queue (nil: none)
// and whether the key's pending entries are cancelled first.
type ScheduleStep struct {
	State         *NotifyState
	Entry         *OutboxEntry
	CancelPending bool
}

// OutboxStore keeps the outbox and the per-channel alert state. In fleet
// and meta-DB mode it is the control pool, like the rules (alerting-06).
type OutboxStore interface {
	// Schedule runs fn on key's current state (nil when none) while no
	// other Schedule of key can run, then writes its step. It reports
	// whether an entry was queued.
	Schedule(ctx context.Context, key StateKey,
		fn func(cur *NotifyState) ScheduleStep) (bool, error)
	// Enqueue queues an entry that keeps no state.
	Enqueue(ctx context.Context, e OutboxEntry) error
	// ClaimDue leases up to limit entries due at now to this caller.
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]OutboxEntry, error)
	// FinishAttempt records one attempt's outcome and ends the lease.
	FinishAttempt(ctx context.Context, id int64, o AttemptOutcome) error
	// DueReminders lists, oldest first, the open states whose severity has
	// a cutoff and that were last delivered at or before it. A severity
	// without a cutoff never reminds, so its states are not listed.
	DueReminders(ctx context.Context, cutoffs map[string]time.Time,
		limit int) ([]NotifyState, error)
	// PurgeOutbox deletes finished entries and closed states older than before.
	PurgeOutbox(ctx context.Context, before time.Time) error
}

// Store is everything the dispatcher reads and writes: rules, channels,
// the delivery log, the outbox and the alert state.
type Store interface {
	RuleStore
	DeliveryRecorder
	OutboxStore
}

// storedEvent is an Event as it is kept in the outbox and in the state.
type storedEvent struct {
	Type     string         `json:"type"`
	Severity string         `json:"severity"`
	Subject  string         `json:"subject"`
	Body     string         `json:"body,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	DedupKey string         `json:"dedup_key,omitempty"`
	Resolve  bool           `json:"resolve,omitempty"`
	RefID    string         `json:"ref_id,omitempty"`
}

func encodeEvent(e Event) ([]byte, error) {
	b, err := json.Marshal(storedEvent(e))
	if err != nil {
		return nil, fmt.Errorf("encode %s event: %w", e.Type, err)
	}
	return b, nil
}

func decodeEvent(b []byte) (Event, error) {
	var s storedEvent
	if err := json.Unmarshal(b, &s); err != nil {
		return Event{}, fmt.Errorf("decode stored event: %w", err)
	}
	return Event(s), nil
}

// syncOnlyData are Data keys holding typed values (card references,
// tokens, reply ids) that do not survive the outbox's JSON: an event
// carrying one is delivered at once, as before.
var syncOnlyData = []string{dataApprovalCard, dataFactCard, DataCardToken, DataFactToken,
	dataPlanCard, DataPlanToken, DataReplyTo}

// queueable reports whether evt may wait in the outbox.
func queueable(evt Event) bool {
	return !syncEvents[evt.Type] && !carriesSyncData(evt)
}

// carriesSyncData reports whether evt holds a typed value the outbox
// cannot keep (a card reference, a token, a reply id).
func carriesSyncData(evt Event) bool {
	for _, k := range syncOnlyData {
		if _, ok := evt.Data[k]; ok {
			return true
		}
	}
	return false
}

// eventDatabase is the database an event concerns, "" when none.
func eventDatabase(evt Event) string {
	db, ok := evt.Data["database"]
	if !ok || db == nil {
		return ""
	}
	return fmt.Sprintf("%v", db)
}
