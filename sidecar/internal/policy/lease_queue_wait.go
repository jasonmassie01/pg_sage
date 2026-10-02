package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// queueMutexKey serializes the depth check and insert of new entries.
var queueMutexKey = advisoryObjectKey("pg_sage:lease_queue")

// enqueue adds a waiting entry behind every earlier one, refusing a full
// queue or a second live entry for the same request.
func (q *LeaseQueue) enqueue(
	ctx context.Context, req QueuedLeaseRequest, requestKey string, keys []string,
) (int64, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("enqueue lease request: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", queueMutexKey); err != nil {
		return 0, fmt.Errorf("enqueue lease request: %w", err)
	}
	var perTarget, total int
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT
		count(*) FILTER (WHERE object_keys && $1), count(*)
		FROM sage.lease_queue
		WHERE state='waiting' AND deadline_at > now()
		  AND COALESCE(database_id, 0) = COALESCE($2::bigint, 0)`,
		keys, q.databaseID).Scan(&perTarget, &total); err != nil {
		return 0, fmt.Errorf("read lease queue depth: %w", err)
	}
	if perTarget >= q.cfg.MaxDepthPerTarget || total >= q.cfg.MaxDepth {
		return 0, fmt.Errorf("%w: %d waiting for these objects, %d in total "+
			"(limits %d and %d)", ErrLeaseQueueFull, perTarget, total,
			q.cfg.MaxDepthPerTarget, q.cfg.MaxDepth)
	}
	id, err := q.insertEntry(ctx, tx, req, requestKey, keys)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("enqueue lease request: %w", err)
	}
	return id, nil
}

func (q *LeaseQueue) insertEntry(
	ctx context.Context, tx pgx.Tx, req QueuedLeaseRequest, requestKey string,
	keys []string,
) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.lease_queue
		(database_id, request_key, object_keys, kind, actor, intent, decision_id,
		 instance, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9))
		ON CONFLICT DO NOTHING RETURNING id`, q.databaseID, requestKey, keys, req.Kind,
		req.Actor, req.Intent, req.Manager.decisionID, q.instance,
		q.maxWait(req).Seconds()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLeaseQueueDuplicate
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue lease request: %w", err)
	}
	return id, nil
}

// wait polls until the entry is first in line for every object and the
// lease is free, its deadline passes, or the caller gives up.
func (q *LeaseQueue) wait(ctx context.Context, req QueuedLeaseRequest, id int64,
) (LeaseID, error) {
	ticker := time.NewTicker(q.cfg.PollInterval)
	defer ticker.Stop()
	var lastConflict error
	for {
		ahead, expired, err := q.position(ctx, id)
		if err != nil {
			return "", q.leave(ctx, id, "cancelled", callerError(ctx, err))
		}
		if expired {
			return "", q.leave(ctx, id, "timed_out", queueTimeout(lastConflict, ahead))
		}
		if ahead == 0 {
			leaseID, err := req.Manager.AcquireTyped(ctx, req.Actor, req.Targets, req.Intent)
			if err == nil {
				return q.grant(ctx, req, id, leaseID)
			}
			if !retryableLeaseError(err) {
				return "", q.leave(ctx, id, "cancelled", callerError(ctx, err))
			}
			lastConflict = err
		}
		select {
		case <-ctx.Done():
			return "", q.leave(ctx, id, "cancelled", callerError(ctx, nil))
		case <-ticker.C:
		}
	}
}

// grant records the entry as granted; if that fails the lease is given back
// so a lease is never held by an entry the queue still shows as waiting.
func (q *LeaseQueue) grant(
	ctx context.Context, req QueuedLeaseRequest, id int64, leaseID LeaseID,
) (LeaseID, error) {
	err := q.resolve(ctx, id, "granted")
	if err == nil {
		return leaseID, nil
	}
	if releaseErr := req.Manager.ReleaseLease(ctx, leaseID); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	return "", err
}

// callerError reports the caller's own cancellation as such: a query that
// failed because the caller gave up is not a database failure.
func callerError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("lease queue wait: %w", ctx.Err())
	}
	return err
}

func queueTimeout(lastConflict error, ahead int) error {
	if lastConflict != nil {
		return fmt.Errorf("%w: %w", ErrLeaseQueueTimeout, lastConflict)
	}
	return fmt.Errorf("%w: %d earlier requests still waiting", ErrLeaseQueueTimeout, ahead)
}

// position heartbeats the entry and counts live entries ahead of it that
// share an object. One statement (one now()) also resolves every other
// entry past its deadline, so an entry that stops counting as ahead is
// recorded as timed out at the same instant. An entry already resolved
// (swept by another waiter) or past its own deadline is expired.
func (q *LeaseQueue) position(ctx context.Context, id int64) (int, bool, error) {
	var ahead int
	var expired bool
	err := q.pool.QueryRow(ctx, `/* pg_sage */ WITH swept AS (
		UPDATE sage.lease_queue SET state='timed_out', resolved_at=now()
		WHERE state='waiting' AND deadline_at <= now() AND id <> $1
		  AND COALESCE(database_id, 0) = COALESCE($2::bigint, 0)
		RETURNING id), me AS (
		UPDATE sage.lease_queue SET heartbeat_at = now()
		WHERE id=$1 AND state='waiting'
		RETURNING id, object_keys, database_id, deadline_at)
		SELECT (SELECT count(*) FROM sage.lease_queue o
		        WHERE o.state='waiting' AND o.deadline_at > now() AND o.id < me.id
		          AND o.object_keys && me.object_keys
		          AND COALESCE(o.database_id, 0) = COALESCE(me.database_id, 0)),
		       me.deadline_at <= now()
		FROM me`, id, q.databaseID).Scan(&ahead, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read lease queue position: %w", err)
	}
	return ahead, expired, nil
}

// leave resolves the entry and returns cause, joined with any failure to
// record the resolution.
func (q *LeaseQueue) leave(ctx context.Context, id int64, state string, cause error) error {
	if err := q.resolve(ctx, id, state); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// resolve ends a waiting entry; one already resolved (swept) is left as is.
func (q *LeaseQueue) resolve(ctx context.Context, id int64, state string) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), LeaseConnectionWait)
	defer cancel()
	_, err := q.pool.Exec(recordCtx, `/* pg_sage */ UPDATE sage.lease_queue
		SET state=$2, resolved_at=now() WHERE id=$1 AND state='waiting'`, id, state)
	if err != nil {
		return fmt.Errorf("resolve lease queue entry %d as %s: %w", id, state, err)
	}
	return nil
}
