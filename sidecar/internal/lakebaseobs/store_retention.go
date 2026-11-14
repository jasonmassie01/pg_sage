package lakebaseobs

import (
	"context"
	"encoding/hex"
	"errors"
	"time"
)

// EnforceRetention also tightens copies from an earlier payload/source policy
// epoch. Increasing a policy never resurrects already erased content.
func (s *Store) EnforceRetention(ctx context.Context, scopeHash string,
	retention time.Duration) error {
	if len(scopeHash) != 64 || retention <= 0 || retention > SourceRetention {
		return ErrScope
	}
	if _, err := hex.DecodeString(scopeHash); err != nil {
		return ErrScope
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, false)
	if err != nil {
		return err
	}
	defer rollbackStoreOperation(tx)
	_, err = tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox
SET payload_until = occurred_at + make_interval(secs => $2)
WHERE binding LIKE $1 AND payload_until > occurred_at + make_interval(secs => $2)`,
		scopeHash+":%", retention.Seconds())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Purge erases payloads at event-time expiry but preserves identities/ACKs for
// the entire seven-day replay horizon plus clock/delivery margin. Small committed
// batches release the shared control pool between batches and make progress even
// when the backlog exceeds one operation's budget. ErrRetentionPending requests
// another scheduled pass; collection and the independent erasure worker both
// retry. Pending, History, delivery and evidence refuse expired payloads even
// while physical erasure remains pending. No locked rows are silently skipped.
func (s *Store) Purge(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	for {
		more, err := s.purgeBatch(ctx, now)
		if err != nil {
			return errors.Join(ErrRetentionPending, err)
		}
		if !more {
			return nil
		}
	}
}

func (s *Store) purgeBatch(ctx context.Context, now time.Time) (bool, error) {
	tx, err := beginStoreOperation(ctx, s.pool, false)
	if err != nil {
		return false, err
	}
	defer rollbackStoreOperation(tx)
	erased, err := tx.Exec(ctx, `WITH expired AS (
 SELECT binding,event_id FROM sage.lakebaseobs_inbox
 WHERE event IS NOT NULL AND payload_until<=$1
 ORDER BY payload_until,binding,event_id LIMIT $2 FOR UPDATE)
UPDATE sage.lakebaseobs_inbox i SET event=NULL FROM expired e
WHERE i.binding=e.binding AND i.event_id=e.event_id`, now, purgeBatchSize)
	if err != nil {
		return false, err
	}
	deleted, err := tx.Exec(ctx, `WITH expired AS (
 SELECT binding,event_id FROM sage.lakebaseobs_inbox WHERE identity_until<=$1
 ORDER BY identity_until,binding,event_id LIMIT $2 FOR UPDATE)
DELETE FROM sage.lakebaseobs_inbox i USING expired e
WHERE i.binding=e.binding AND i.event_id=e.event_id`, now, purgeBatchSize)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return erased.RowsAffected() == purgeBatchSize || deleted.RowsAffected() == purgeBatchSize, nil
}
