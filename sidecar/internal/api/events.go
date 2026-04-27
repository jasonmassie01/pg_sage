package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// EventType enumerates the resource changes the SSE broker can publish.
// Clients subscribe to a single stream and filter client-side by type.
type EventType string

const (
	EventFindings  EventType = "findings"
	EventActions   EventType = "actions"
	EventHealth    EventType = "health"
	EventHeartbeat EventType = "heartbeat"
)

// Event is the envelope sent to every subscriber.
type Event struct {
	Type     EventType `json:"type"`
	Database string    `json:"database,omitempty"`
	Payload  any       `json:"payload,omitempty"`
	Ts       time.Time `json:"ts"`
}

// EventBroker fans out resource-change events to any number of SSE
// subscribers. Subscribers are per-client buffered channels; a slow
// subscriber is dropped on overflow rather than stalling the broker.
type EventBroker struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
	started     atomic.Bool
	stopCh      chan struct{}
}

// NewEventBroker constructs an unstarted broker. Call Start to begin
// background polling.
func NewEventBroker() *EventBroker {
	return &EventBroker{
		subscribers: make(map[chan Event]struct{}),
		stopCh:      make(chan struct{}),
	}
}

// Subscribe registers a new listener. The returned cancel function
// drops the subscription and closes the channel.
func (b *EventBroker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subscribers[ch]; ok {
			delete(b.subscribers, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish fans an event out to all current subscribers. Non-blocking:
// a full channel causes the event to be dropped for that subscriber.
func (b *EventBroker) Publish(evt Event) {
	if evt.Ts.IsZero() {
		evt.Ts = time.Now().UTC()
	}
	b.mu.RLock()
	for ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
			// slow subscriber; drop.
		}
	}
	b.mu.RUnlock()
}

// SubscriberCount returns the current number of active subscribers.
// Primarily used by tests.
func (b *EventBroker) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Start kicks off the polling + heartbeat goroutines.
// pollInterval governs how often the broker scans sage.findings and
// sage.action_log for changes. heartbeatInterval governs the cadence of
// keep-alive frames sent to idle subscribers so proxies don't time
// the connection out.
func (b *EventBroker) Start(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	pollInterval, heartbeatInterval time.Duration,
) {
	if !b.started.CompareAndSwap(false, true) {
		return
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	if heartbeatInterval <= 0 {
		heartbeatInterval = 15 * time.Second
	}
	go b.pollLoop(ctx, mgr, pollInterval)
	go b.heartbeatLoop(ctx, heartbeatInterval)
}

// Stop is safe to call multiple times.
func (b *EventBroker) Stop() {
	select {
	case <-b.stopCh:
	default:
		close(b.stopCh)
	}
}

func (b *EventBroker) heartbeatLoop(
	ctx context.Context, interval time.Duration,
) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
			b.Publish(Event{Type: EventHeartbeat})
		}
	}
}

// lastSeen tracks the most recent (updated_at, count) tuple for a
// resource on a given database. It is the poller's diff key.
type lastSeen struct {
	updated time.Time
	count   int64
}

func (b *EventBroker) pollLoop(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	interval time.Duration,
) {
	t := time.NewTicker(interval)
	defer t.Stop()

	// {database -> resource -> lastSeen}
	state := map[string]map[EventType]lastSeen{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-t.C:
			b.pollOnce(ctx, mgr, state)
		}
	}
}

func (b *EventBroker) pollOnce(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	state map[string]map[EventType]lastSeen,
) {
	if mgr == nil {
		return
	}
	for name, inst := range mgr.Instances() {
		if inst == nil || inst.Pool == nil {
			continue
		}
		if _, ok := state[name]; !ok {
			state[name] = map[EventType]lastSeen{}
		}
		b.pollResource(
			ctx, inst.Pool, name, EventFindings,
			`SELECT
				coalesce(max(greatest(
					created_at,
					last_seen,
					coalesce(resolved_at, 'epoch'::timestamptz),
					coalesce(acted_on_at, 'epoch'::timestamptz)
				)), 'epoch'::timestamptz),
				count(*)
			 FROM sage.findings`,
			state[name],
		)
		b.pollResource(
			ctx, inst.Pool, name, EventActions,
			`SELECT
				coalesce(max(ts), 'epoch'::timestamptz),
				count(*)
			 FROM (
				SELECT greatest(
					executed_at,
					coalesce(measured_at, 'epoch'::timestamptz)
				) AS ts
				FROM sage.action_log
				UNION ALL
				SELECT greatest(
					proposed_at,
					coalesce(decided_at, 'epoch'::timestamptz)
				) AS ts
				FROM sage.action_queue
			 ) action_events`,
			state[name],
		)
		b.pollResource(
			ctx, inst.Pool, name, EventHealth,
			`SELECT
				coalesce(max(recorded_at), 'epoch'::timestamptz),
				count(*)
			 FROM sage.health_history`,
			state[name],
		)
	}
}

func (b *EventBroker) pollResource(
	ctx context.Context,
	pool *pgxpool.Pool,
	database string,
	typ EventType,
	query string,
	dbState map[EventType]lastSeen,
) {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var ts time.Time
	var count int64
	if err := pool.QueryRow(qctx, query).Scan(&ts, &count); err != nil {
		// Expected during bootstrap (table missing) or if the pool
		// was just torn down. Don't spam logs.
		slog.Debug("sse poll resource",
			"database", database, "resource", typ, "error", err)
		return
	}
	prev := dbState[typ]
	if ts.Equal(prev.updated) && count == prev.count {
		return
	}
	dbState[typ] = lastSeen{updated: ts, count: count}
	// Don't publish on the first observation — we have no baseline
	// to compare, so treating it as a change would cause every
	// client to refetch on connect.
	if prev.updated.IsZero() && prev.count == 0 {
		return
	}
	b.Publish(Event{
		Type:     typ,
		Database: database,
		Payload: map[string]any{
			"count":      count,
			"updated_at": ts,
		},
	})
}

// eventsHandler streams SSE frames from the broker. Uses
// response flushing rather than buffering so events reach
// the client within the poll cadence.
func eventsHandler(b *EventBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, "streaming not supported",
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// Greet with a retry hint and an initial heartbeat so the
		// client's EventSource fires onopen quickly.
		fmt.Fprintf(w, "retry: 3000\n\n")
		flusher.Flush()

		ch, cancel := b.Subscribe()
		defer cancel()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case evt, open := <-ch:
				if !open {
					return
				}
				data, err := json.Marshal(evt)
				if err != nil {
					continue
				}
				// "event:" lets client code dispatch per type if
				// it prefers addEventListener over onmessage.
				fmt.Fprintf(w, "event: %s\n", evt.Type)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}
