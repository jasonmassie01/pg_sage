package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrLadderQueueNotFound: no queue row with that id.
var ErrLadderQueueNotFound = errors.New("verification ladder: queue item not found")

// LadderDB is a pool, connection or transaction on pg_sage's own schema.
type LadderDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// AppendLadder appends recs to a queue row's ladder in one UPDATE, so
// producers appending at once never overwrite each other. No records is a
// no-op.
func AppendLadder(ctx context.Context, db LadderDB, queueID int,
	recs ...LadderRecord) error {
	if len(recs) == 0 {
		return nil
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		return fmt.Errorf("encode ladder for queue item %d: %w", queueID, err)
	}
	tag, err := db.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET ladder = COALESCE(ladder, '[]'::jsonb) || $2::jsonb WHERE id = $1`, queueID, raw)
	if err != nil {
		return fmt.Errorf("append ladder to queue item %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %d", ErrLadderQueueNotFound, queueID)
	}
	return nil
}

// AppendLadderForAction appends recs to the ladder of the queue rows that
// ran action actionID and reports how many it updated (0: the action ran
// without a queue item, autonomously).
func AppendLadderForAction(ctx context.Context, db LadderDB, actionID int64,
	recs ...LadderRecord) (int64, error) {
	if len(recs) == 0 {
		return 0, nil
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		return 0, fmt.Errorf("encode ladder for action %d: %w", actionID, err)
	}
	tag, err := db.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET ladder = COALESCE(ladder, '[]'::jsonb) || $2::jsonb
		WHERE action_log_id = $1`, actionID, raw)
	if err != nil {
		return 0, fmt.Errorf("append ladder for action %d: %w", actionID, err)
	}
	return tag.RowsAffected(), nil
}

// ReadLadder returns a queue row's stored ladder as JSON (nil for NULL: a
// row queued before the ladder existed or with nothing recorded).
func ReadLadder(ctx context.Context, db LadderDB, queueID int) ([]byte, error) {
	var raw []byte
	err := db.QueryRow(ctx, `/* pg_sage */ SELECT ladder FROM sage.action_queue
		WHERE id = $1`, queueID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrLadderQueueNotFound, queueID)
	}
	if err != nil {
		return nil, fmt.Errorf("read ladder of queue item %d: %w", queueID, err)
	}
	return raw, nil
}
