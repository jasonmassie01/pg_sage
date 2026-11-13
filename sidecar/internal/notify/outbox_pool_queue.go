package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertEntry(ctx context.Context, db execer, e OutboxEntry) error {
	if _, err := db.Exec(ctx, `/* pg_sage */ INSERT INTO sage.notification_outbox
		(database, dedup_key, channel_id, event, payload, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.Database, e.DedupKey, e.ChannelID, e.Event, string(e.Payload),
		e.NextAttemptAt); err != nil {
		return fmt.Errorf("queue %s notification for channel %d: %w", e.Event, e.ChannelID, err)
	}
	return nil
}

// Enqueue queues an entry that keeps no alert state.
func (s *PoolStore) Enqueue(ctx context.Context, e OutboxEntry) error {
	if s.pool == nil {
		return fmt.Errorf("queue notification: notification pool is nil")
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return insertEntry(qctx, s.pool, e)
}

// ClaimDue leases due entries with SKIP LOCKED, so sidecars sharing the
// control database never claim the same entry; an entry whose lease ran
// out (its claimer died) is due again.
func (s *PoolStore) ClaimDue(
	ctx context.Context, now time.Time, limit int,
) ([]OutboxEntry, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("claim notifications: notification pool is nil")
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ UPDATE sage.notification_outbox o
		SET status = 'sending', claimed_until = $1::timestamptz + $3::interval
		WHERE o.id IN (
		    SELECT id FROM sage.notification_outbox
		     WHERE (status = 'pending' AND next_attempt_at <= $1)
		        OR (status = 'sending' AND claimed_until <= $1)
		     ORDER BY next_attempt_at, id
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED)
		RETURNING o.id, o.database, o.dedup_key, o.channel_id, o.event, o.payload,
		          o.attempts, o.next_attempt_at, COALESCE(o.last_error, '')`,
		now, limit, outboxLease.String())
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		e := OutboxEntry{Status: "sending"}
		if err := rows.Scan(&e.ID, &e.Database, &e.DedupKey, &e.ChannelID, &e.Event,
			&e.Payload, &e.Attempts, &e.NextAttemptAt, &e.LastError); err != nil {
			return nil, fmt.Errorf("scan claimed notification: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	return out, nil
}

// FinishAttempt records one attempt's outcome and releases the lease.
func (s *PoolStore) FinishAttempt(ctx context.Context, id int64, o AttemptOutcome) error {
	if s.pool == nil {
		return fmt.Errorf("finish notification %d: notification pool is nil", id)
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.notification_outbox
		SET status = $2, attempts = $3, next_attempt_at = $4,
		    last_error = NULLIF($5, ''), claimed_until = NULL
		WHERE id = $1`, id, o.Status, o.Attempts, o.NextAttemptAt, o.LastError)
	if err != nil {
		return fmt.Errorf("finish notification %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish notification %d: entry not found", id)
	}
	return nil
}

// DueReminders lists, oldest first, the open alert states last delivered
// at or before their severity's cutoff; severities without one are skipped.
func (s *PoolStore) DueReminders(
	ctx context.Context, cutoffs map[string]time.Time, limit int,
) ([]NotifyState, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("list due reminders: notification pool is nil")
	}
	if len(cutoffs) == 0 {
		return nil, nil
	}
	sevs := make([]string, 0, len(cutoffs))
	ats := make([]time.Time, 0, len(cutoffs))
	for sev, at := range cutoffs {
		sevs, ats = append(sevs, sev), append(ats, at)
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+stateColumns+`
		FROM sage.notification_state
		JOIN unnest($1::text[], $2::timestamptz[]) AS c(sev, cutoff)
		  ON severity = c.sev AND last_sent_at <= c.cutoff
		WHERE open
		ORDER BY last_sent_at LIMIT $3`, sevs, ats, limit)
	if err != nil {
		return nil, fmt.Errorf("list due reminders: %w", err)
	}
	defer rows.Close()
	var out []NotifyState
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan notification state: %w", err)
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// PurgeOutbox deletes finished entries and closed states older than before.
func (s *PoolStore) PurgeOutbox(ctx context.Context, before time.Time) error {
	if s.pool == nil {
		return fmt.Errorf("purge notifications: notification pool is nil")
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	if _, err := s.pool.Exec(qctx, `/* pg_sage */ DELETE FROM sage.notification_outbox
		WHERE status IN ('sent', 'failed', 'cancelled') AND next_attempt_at < $1`,
		before); err != nil {
		return fmt.Errorf("purge notification outbox: %w", err)
	}
	if _, err := s.pool.Exec(qctx, `/* pg_sage */ DELETE FROM sage.notification_state
		WHERE NOT open AND last_sent_at < $1
		  AND (resolved_at IS NULL OR resolved_at < $1)`, before); err != nil {
		return fmt.Errorf("purge notification state: %w", err)
	}
	return nil
}

var _ Store = (*PoolStore)(nil)
