package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// outboxLease is how long a claimed entry stays with its claimer; a
// sidecar that dies mid-send leaves it to another after the lease.
const outboxLease = 2 * time.Minute

const stateColumns = `database, dedup_key, channel_id, open, severity,
	first_sent_at, last_sent_at, resolved_at, reminders, payload`

func (s *PoolStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

// Schedule takes a transaction-scoped advisory lock on key, so two
// sidecars sharing the control database decide one alert in turn.
func (s *PoolStore) Schedule(
	ctx context.Context, key StateKey, fn func(cur *NotifyState) ScheduleStep,
) (bool, error) {
	if s.pool == nil {
		return false, fmt.Errorf("schedule notification: notification pool is nil")
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return false, fmt.Errorf("schedule notification: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	lockKey := fmt.Sprintf("notify|%s|%s|%d", key.Database, key.DedupKey, key.ChannelID)
	if _, err := tx.Exec(qctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
		hashtextextended($1, 0))`, lockKey); err != nil {
		return false, fmt.Errorf("schedule notification: lock: %w", err)
	}
	cur, err := readState(qctx, tx, key)
	if err != nil {
		return false, err
	}
	step := fn(cur)
	if err := applyStep(qctx, tx, key, step); err != nil {
		return false, err
	}
	if err := tx.Commit(qctx); err != nil {
		return false, fmt.Errorf("schedule notification: commit: %w", err)
	}
	return step.Entry != nil, nil
}

func readState(ctx context.Context, tx pgx.Tx, key StateKey) (*NotifyState, error) {
	row := tx.QueryRow(ctx, `/* pg_sage */ SELECT `+stateColumns+`
		FROM sage.notification_state
		WHERE database = $1 AND dedup_key = $2 AND channel_id = $3`,
		key.Database, key.DedupKey, key.ChannelID)
	st, err := scanState(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read notification state: %w", err)
	}
	return st, nil
}

func scanState(row pgx.Row) (*NotifyState, error) {
	var st NotifyState
	var first, last, resolved *time.Time
	if err := row.Scan(&st.Key.Database, &st.Key.DedupKey, &st.Key.ChannelID, &st.Open,
		&st.Severity, &first, &last, &resolved, &st.Reminders, &st.Payload); err != nil {
		return nil, err
	}
	for _, p := range []struct {
		dst *time.Time
		src *time.Time
	}{{&st.FirstSentAt, first}, {&st.LastSentAt, last}, {&st.ResolvedAt, resolved}} {
		if p.src != nil {
			*p.dst = p.src.UTC()
		}
	}
	return &st, nil
}

func applyStep(ctx context.Context, tx pgx.Tx, key StateKey, step ScheduleStep) error {
	if step.CancelPending {
		if _, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.notification_outbox
			SET status = 'cancelled'
			WHERE database = $1 AND dedup_key = $2 AND channel_id = $3
			  AND status = 'pending'`, key.Database, key.DedupKey, key.ChannelID); err != nil {
			return fmt.Errorf("cancel pending notifications: %w", err)
		}
	}
	if st := step.State; st != nil {
		if _, err := tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.notification_state
			(`+stateColumns+`, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, clock_timestamp())
			ON CONFLICT (database, dedup_key, channel_id) DO UPDATE SET
			  open = EXCLUDED.open, severity = EXCLUDED.severity,
			  first_sent_at = EXCLUDED.first_sent_at, last_sent_at = EXCLUDED.last_sent_at,
			  resolved_at = EXCLUDED.resolved_at, reminders = EXCLUDED.reminders,
			  payload = EXCLUDED.payload, updated_at = EXCLUDED.updated_at`,
			key.Database, key.DedupKey, key.ChannelID, st.Open, st.Severity,
			nullTime(st.FirstSentAt), nullTime(st.LastSentAt), nullTime(st.ResolvedAt),
			st.Reminders, nullJSON(st.Payload)); err != nil {
			return fmt.Errorf("write notification state: %w", err)
		}
	}
	if step.Entry != nil {
		return insertEntry(ctx, tx, *step.Entry)
	}
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
