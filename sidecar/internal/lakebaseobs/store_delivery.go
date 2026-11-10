package lakebaseobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Select identities before reading JSON documents: an ACK-heavy backlog can
// walk the covering index without fetching every already delivered payload.
// There is no consumer watermark: a late historical event is still eligible.
// An event waiting for its next retry is skipped until next_at.
const pendingTemplate = `WITH pending AS MATERIALIZED (
 SELECT i.binding,i.event_id,i.occurred_at FROM sage.lakebaseobs_inbox i
 WHERE i.binding=$1 AND i.event IS NOT NULL AND i.payload_until>$3%s
 AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_acks a
  WHERE a.binding=i.binding AND a.event_id=i.event_id AND a.consumer=$2)
 AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_attempts t
  WHERE t.binding=i.binding AND t.event_id=i.event_id AND t.consumer=$2 AND t.next_at>$3)
 ORDER BY i.occurred_at,i.event_id LIMIT $4)
SELECT (SELECT i.event FROM sage.lakebaseobs_inbox i
 WHERE i.binding=p.binding AND i.event_id=p.event_id)
FROM pending p ORDER BY p.occurred_at,p.event_id`

// backlogTemplate counts what Pending could still return, including events
// waiting for a retry, oldest first so the bound never hides the oldest row.
const backlogTemplate = `WITH p AS (
 SELECT i.occurred_at FROM sage.lakebaseobs_inbox i
 WHERE i.binding=$1 AND i.event IS NOT NULL AND i.payload_until>$3%s
 AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_acks a
  WHERE a.binding=i.binding AND a.event_id=i.event_id AND a.consumer=$2)
 ORDER BY i.occurred_at,i.event_id LIMIT $4)
SELECT count(*),min(occurred_at) FROM p`

const streamFilter = " AND i.stream=ANY($5::text[])"

var (
	pendingSQL        = fmt.Sprintf(pendingTemplate, "")
	pendingStreamsSQL = fmt.Sprintf(pendingTemplate, streamFilter)
	backlogSQL        = fmt.Sprintf(backlogTemplate, "")
	backlogStreamsSQL = fmt.Sprintf(backlogTemplate, streamFilter)
)

// recordFailureSQL counts one failed attempt and schedules the next one. It
// records nothing for an event whose identity was already purged.
const recordFailureSQL = `INSERT INTO sage.lakebaseobs_attempts AS t
 (binding,event_id,consumer,attempts,next_at,last_error)
SELECT i.binding,i.event_id,$3,1,$4::timestamptz+make_interval(secs=>$5),$7
FROM sage.lakebaseobs_inbox i WHERE i.binding=$1 AND i.event_id=$2
ON CONFLICT(binding,event_id,consumer) DO UPDATE SET attempts=t.attempts+1,
 next_at=$4::timestamptz+make_interval(secs=>LEAST($5*power(2,t.attempts),$6)),
 last_error=EXCLUDED.last_error
RETURNING attempts`

const insertEventSQL = `INSERT INTO sage.lakebaseobs_inbox
 (binding,event_id,stream,occurred_at,received_at,payload_until,identity_until,event)
VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)ON CONFLICT(binding,event_id)DO NOTHING`

// CommitBatchAcked is CommitBatch that also acknowledges, for each named
// consumer, every event this commit newly stores, in the same transaction and
// under the same shared policy fence. It returns those newly stored events;
// an event already in the inbox is neither returned nor acknowledged again.
func (s *Store) CommitBatchAcked(ctx context.Context, binding string, stream Stream,
	before, after Cursor, events []Event, retention time.Duration, consumers []string,
) ([]Event, error) {
	if binding == "" || retention <= 0 || retention > SourceRetention {
		return nil, ErrScope
	}
	for _, c := range consumers {
		if !consumerName.MatchString(c) {
			return nil, ErrScope
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.lockDeliveryPolicy(ctx, tx, binding, events); err != nil {
		return nil, err
	}
	if err = lockCursor(ctx, tx, binding, stream, before.Revision); err != nil {
		return nil, err
	}
	inserted, err := insertEvents(ctx, tx, binding, stream, events, retention)
	if err != nil {
		return nil, err
	}
	if err = ackInserted(ctx, tx, binding, inserted, consumers); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE sage.lakebaseobs_cursors SET revision=revision+1,
state=$3::jsonb,updated_at=now() WHERE binding=$1 AND stream=$2`, binding, string(stream), raw)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return inserted, nil
}

// lockCursor creates the stream's cursor row if needed, locks it, and fails
// with ErrConflict when another writer advanced it since it was loaded.
func lockCursor(
	ctx context.Context, tx pgx.Tx, binding string, stream Stream, revision int64,
) error {
	_, err := tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_cursors(binding,stream,revision,state)
VALUES($1,$2,0,'{}')ON CONFLICT DO NOTHING`, binding, string(stream))
	if err != nil {
		return err
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM sage.lakebaseobs_cursors
WHERE binding=$1 AND stream=$2 FOR UPDATE`, binding, string(stream)).Scan(&current)
	if err != nil {
		return err
	}
	if current != revision {
		return ErrConflict
	}
	return nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, binding string, stream Stream, events []Event,
	retention time.Duration,
) ([]Event, error) {
	var inserted []Event
	for _, e := range events {
		if e.ID == "" || e.Stream != stream || e.Version != SchemaVersion ||
			e.OccurredAt.IsZero() || e.ReceivedAt.IsZero() {
			return nil, ErrMalformed
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		identityUntil := e.OccurredAt.Add(SourceRetention + 24*time.Hour)
		tag, err := tx.Exec(ctx, insertEventSQL, binding, e.ID, string(stream), e.OccurredAt,
			e.ReceivedAt, e.OccurredAt.Add(retention), identityUntil, raw)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			inserted = append(inserted, e)
		}
	}
	return inserted, nil
}

func ackInserted(
	ctx context.Context, tx pgx.Tx, binding string, inserted []Event, consumers []string,
) error {
	if len(inserted) == 0 {
		return nil
	}
	ids := make([]string, len(inserted))
	for i, e := range inserted {
		ids[i] = e.ID
	}
	for _, c := range consumers {
		_, err := tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer)
SELECT $1,unnest($2::text[]),$3 ON CONFLICT DO NOTHING`, binding, ids, c)
		if err != nil {
			return err
		}
	}
	return nil
}

// streamNames validates a subscription; nil or empty means every stream.
func streamNames(streams []Stream) ([]string, error) {
	if len(streams) == 0 {
		return nil, nil
	}
	out := make([]string, len(streams))
	for i, s := range streams {
		if _, ok := specs[s]; !ok {
			return nil, ErrScope
		}
		out[i] = string(s)
	}
	return out, nil
}

func (s *Store) Pending(
	ctx context.Context, binding, consumer string, limit int, now time.Time,
) ([]Event, error) {
	return s.PendingStreams(ctx, binding, consumer, nil, limit, now)
}

// PendingStreams returns up to limit unacknowledged events of the listed
// streams (all streams when nil), oldest first, skipping those waiting for a
// retry until their next attempt is due.
func (s *Store) PendingStreams(ctx context.Context, binding, consumer string, streams []Stream,
	limit int, now time.Time,
) ([]Event, error) {
	if !consumerName.MatchString(consumer) || limit < 1 || limit > 10000 {
		return nil, ErrScope
	}
	names, err := streamNames(streams)
	if err != nil {
		return nil, err
	}
	query, args := pendingSQL, []any{binding, consumer, now, limit}
	if names != nil {
		query, args = pendingStreamsSQL, append(args, names)
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, true)
	if err != nil {
		return nil, err
	}
	defer rollbackStoreOperation(tx)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var raw []byte
		var e Event
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &e) != nil {
			return nil, ErrSchema
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordDeliveryFailure counts a failed delivery and returns the attempts so
// far, or 0 when the event no longer exists. At MaxDeliveryAttempts it also
// acknowledges the event, in the same transaction, so it is not offered again.
// class must be a fixed failure class, never error text.
func (s *Store) RecordDeliveryFailure(ctx context.Context, binding, eventID, consumer, class string,
	now time.Time,
) (int, error) {
	if binding == "" || eventID == "" || !consumerName.MatchString(consumer) {
		return 0, ErrScope
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, false)
	if err != nil {
		return 0, err
	}
	defer rollbackStoreOperation(tx)
	var attempts int
	err = tx.QueryRow(ctx, recordFailureSQL, binding, eventID, consumer, now,
		retryBaseDelay.Seconds(), retryMaxDelay.Seconds(), class).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if attempts >= MaxDeliveryAttempts {
		if _, err = tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer)
VALUES($1,$2,$3)ON CONFLICT DO NOTHING`, binding, eventID, consumer); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return attempts, nil
}

// Backlog counts the consumer's deliverable, unacknowledged events of the
// listed streams (all when nil), bounded by backlogLimit.
func (s *Store) Backlog(ctx context.Context, binding, consumer string, streams []Stream,
	now time.Time,
) (Backlog, error) {
	if binding == "" || !consumerName.MatchString(consumer) {
		return Backlog{}, ErrScope
	}
	names, err := streamNames(streams)
	if err != nil {
		return Backlog{}, err
	}
	query, args := backlogSQL, []any{binding, consumer, now, backlogLimit}
	if names != nil {
		query, args = backlogStreamsSQL, append(args, names)
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, true)
	if err != nil {
		return Backlog{}, err
	}
	defer rollbackStoreOperation(tx)
	var b Backlog
	var oldest *time.Time
	if err = tx.QueryRow(ctx, query, args...).Scan(&b.Rows, &oldest); err != nil {
		return Backlog{}, err
	}
	b.Capped = b.Rows >= backlogLimit
	if oldest != nil {
		b.Oldest = oldest.UTC()
	}
	return b, nil
}
