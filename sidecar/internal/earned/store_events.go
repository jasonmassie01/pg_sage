package earned

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EventType is one kind of ledger history entry.
type EventType string

// Ledger history. Every level change, proposal decision, transient cap
// and L3 auto-execution is recorded with its actor and reason.
const (
	EventPromotionProposed EventType = "promotion_proposed"
	EventPromotionApproved EventType = "promotion_approved"
	EventPromotionRejected EventType = "promotion_rejected"
	EventPromotionExpired  EventType = "promotion_expired"
	EventDowngraded        EventType = "downgraded"
	EventAutoDowngraded    EventType = "auto_downgraded"
	EventCapped            EventType = "capped"
	EventCapCleared        EventType = "cap_cleared"
	EventAutoExecuted      EventType = "auto_executed"
	// EventCarriedOver seeds a pair at the level the policy before M7
	// already granted it; EventDeadlineOverride records a mandatory
	// deadline action the ledger did not restrict.
	EventCarriedOver      EventType = "carried_over"
	EventDeadlineOverride EventType = "deadline_override"
)

// Event is one ledger history entry.
type Event struct {
	ID          int64           `json:"id"`
	Family      Family          `json:"family"`
	Class       ActionClass     `json:"class"`
	Type        EventType       `json:"type"`
	From        *Level          `json:"from,omitempty"`
	To          *Level          `json:"to,omitempty"`
	Actor       string          `json:"actor"`
	Reason      string          `json:"reason"`
	Database    string          `json:"database,omitempty"`
	ProposalID  string          `json:"proposal_id,omitempty"`
	ActionLogID int64           `json:"action_log_id,omitempty"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
	At          time.Time       `json:"at"`
}

// EventFilter narrows the history; zero fields match everything.
type EventFilter struct {
	Family   Family
	Class    ActionClass
	Database string
	// Limit is the page size, 1..MaxEventPage (default 100).
	Limit int
}

// MaxEventPage bounds one history page.
const MaxEventPage = 500

func levelArg(l *Level) any {
	if l == nil {
		return nil
	}
	return int16(*l)
}

func levelPtr(l Level) *Level { return &l }

// appendEvent records one history entry.
func (s *PostgresStore) appendEvent(ctx context.Context, q querier, e Event) error {
	if len(e.Evidence) == 0 {
		e.Evidence = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, from_level, to_level, actor,
		 reason, database_name, proposal_id, action_log_id, evidence, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, '')::uuid,
		        NULLIF($11, 0), $12, $13)`,
		s.deployment, string(e.Family), string(e.Class), string(e.Type), levelArg(e.From),
		levelArg(e.To), e.Actor, e.Reason, e.Database, e.ProposalID, e.ActionLogID,
		e.Evidence, e.At)
	return storeErr("append autonomy event", err)
}

// Events reads the history, newest first.
func (s *PostgresStore) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > MaxEventPage {
		return nil, fmt.Errorf("%w: limit %d (1..%d)", ErrInvalidRequest, f.Limit,
			MaxEventPage)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, family, action_class, event_type,
		from_level, to_level, actor, reason, COALESCE(database_name, ''),
		COALESCE(proposal_id::text, ''), COALESCE(action_log_id, 0), evidence, created_at
		FROM sage.sre_autonomy_events
		WHERE deployment_id = $1 AND ($2 = '' OR family = $2)
		  AND ($3 = '' OR action_class = $3) AND ($4 = '' OR database_name = $4)
		ORDER BY created_at DESC, id DESC LIMIT $5`, s.deployment, string(f.Family),
		string(f.Class), f.Database, f.Limit)
	if err != nil {
		return nil, storeErr("read autonomy history", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, storeErr("scan autonomy event", err)
		}
		out = append(out, e)
	}
	return out, storeErr("read autonomy history", rows.Err())
}

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	var family, class, typ string
	var from, to *int16
	err := row.Scan(&e.ID, &family, &class, &typ, &from, &to, &e.Actor, &e.Reason,
		&e.Database, &e.ProposalID, &e.ActionLogID, &e.Evidence, &e.At)
	e.Family, e.Class, e.Type = Family(family), ActionClass(class), EventType(typ)
	if from != nil {
		e.From = levelPtr(Level(*from))
	}
	if to != nil {
		e.To = levelPtr(Level(*to))
	}
	return e, err
}

// autoExecutedRecorded reports whether an L3 execution was notified.
func (s *PostgresStore) autoExecutedRecorded(ctx context.Context, database string,
	actionLogID int64) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.sre_autonomy_events
		WHERE deployment_id = $1 AND event_type = 'auto_executed'
		  AND database_name = $2 AND action_log_id = $3)`,
		s.deployment, database, actionLogID).Scan(&found)
	return found, storeErr("read auto-execution notice", err)
}
