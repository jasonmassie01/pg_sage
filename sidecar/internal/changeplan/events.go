package changeplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// EventKind is what a plan event records (sage.plan_event.kind).
type EventKind string

// The event vocabulary, set up front: the store's writes (created,
// edited, approved, voided) and the engine's transitions (plan-02).
const (
	EventCreated      EventKind = "created"
	EventProposed     EventKind = "proposed"
	EventEdited       EventKind = "edited"
	EventApproved     EventKind = "approved"
	EventVoided       EventKind = "voided"
	EventRejected     EventKind = "rejected"
	EventStepStarted  EventKind = "step_started"
	EventStepExecuted EventKind = "step_executed"
	EventVerdict      EventKind = "verdict"
	EventStepReverted EventKind = "step_reverted"
	EventHalted       EventKind = "halted"
	EventResumed      EventKind = "resumed"
	EventCompleted    EventKind = "completed"
	EventCancelled    EventKind = "cancelled"
)

var knownEventKinds = map[EventKind]bool{
	EventCreated: true, EventProposed: true, EventEdited: true, EventApproved: true,
	EventVoided: true, EventRejected: true, EventStepStarted: true,
	EventStepExecuted: true, EventVerdict: true, EventStepReverted: true,
	EventHalted: true, EventResumed: true, EventCompleted: true, EventCancelled: true,
}

// Unattributed is the actor of a write whose caller named nobody
// (WithActor): recorded as such, never as pg_sage.
const Unattributed = "unattributed"

// ErrInvalidEvent refuses an event before it reaches the database.
var ErrInvalidEvent = errors.New("changeplan: invalid plan event")

// Event is a sage.plan_event row: an append-only, chained record of one
// plan write or transition, with the plan hash after it.
type Event struct {
	ID        int64          `json:"id,omitempty"`
	PlanID    int64          `json:"plan_id"`
	StepID    *int64         `json:"step_id,omitempty"`
	Kind      EventKind      `json:"kind"`
	PlanHash  string         `json:"plan_hash"`
	Actor     string         `json:"actor"`
	Detail    map[string]any `json:"detail"`
	CreatedAt time.Time      `json:"created_at"`
}

type actorKey struct{}

// WithActor names who makes the store writes run under ctx (a user, an
// agp_ principal); the store records it on their events.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// actorOf is the actor WithActor named, or Unattributed.
func actorOf(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return Unattributed
}

func (e Event) check() error {
	switch {
	case !knownEventKinds[e.Kind]:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidEvent, e.Kind)
	case e.PlanID <= 0:
		return fmt.Errorf("%w: %s event names no plan", ErrInvalidEvent, e.Kind)
	case !packHashPattern.MatchString(e.PlanHash):
		return fmt.Errorf("%w: %s event plan hash %q is not 64 lower-case hex",
			ErrInvalidEvent, e.Kind, e.PlanHash)
	case e.Actor == "" || utf8.RuneCountInString(e.Actor) > maxRefRunes:
		return fmt.Errorf("%w: %s event actor is 1 to %d characters", ErrInvalidEvent,
			e.Kind, maxRefRunes)
	}
	return nil
}

// AppendEvent records e in tx, the transaction of the change it records,
// so neither commits without the other. The plan must exist. The event is
// chained at commit (auditchain.PlanEvents); a refused event fails tx.
func AppendEvent(ctx context.Context, tx pgx.Tx, e Event) (int64, error) {
	if err := e.check(); err != nil {
		return 0, err
	}
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, fmt.Errorf("%w: %s event detail: %v", ErrInvalidEvent, e.Kind, err)
	}
	var id int64
	err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.plan_event (plan_id, step_id,
		kind, plan_hash, actor, detail)
		SELECT $1, $2, $3, $4, $5, $6 WHERE EXISTS (SELECT 1 FROM sage.plan WHERE id = $1)
		RETURNING id`, e.PlanID, e.StepID, string(e.Kind), e.PlanHash, e.Actor,
		raw).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("changeplan: record the %s event: %w: plan %d", e.Kind,
			ErrNotFound, e.PlanID)
	}
	if err != nil {
		return 0, fmt.Errorf("changeplan: record the %s event of plan %d: %w", e.Kind,
			e.PlanID, err)
	}
	return id, nil
}

// Events returns plan planID's events, oldest first.
func (s *Store) Events(ctx context.Context, planID int64) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, plan_id, step_id, kind,
		plan_hash, actor, detail, created_at FROM sage.plan_event WHERE plan_id = $1
		ORDER BY id`, planID)
	if err != nil {
		return nil, fmt.Errorf("changeplan: events of plan %d: %w", planID, err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var kind string
		if err := rows.Scan(&e.ID, &e.PlanID, &e.StepID, &kind, &e.PlanHash, &e.Actor,
			&e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("changeplan: events of plan %d: %w", planID, err)
		}
		e.Kind = EventKind(kind)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("changeplan: events of plan %d: %w", planID, err)
	}
	return out, nil
}
