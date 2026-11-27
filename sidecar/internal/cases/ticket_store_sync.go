package cases

import (
	"context"
	"fmt"
	"time"
)

// DueForSync leases up to limit created tickets that were not synced in
// the last minAge, oldest sync first, by stamping last_synced_at. SKIP
// LOCKED keeps two sidecars sharing the control database from syncing
// one ticket at once. A ticket that is done and already commented on is
// finished and never synced again.
func (s *PgTicketStore) DueForSync(
	ctx context.Context, limit int, minAge time.Duration,
) ([]Ticket, error) {
	qctx, cancel := s.query(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */
		UPDATE sage.case_tickets SET last_synced_at = now()
		WHERE id IN (
		    SELECT id FROM sage.case_tickets
		    WHERE external_key IS NOT NULL
		      AND NOT (status_category = 'done' AND resolved_comment_at IS NOT NULL)
		      AND (last_synced_at IS NULL
		           OR last_synced_at <= now() - make_interval(secs => $2))
		    ORDER BY last_synced_at NULLS FIRST, id
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED)
		RETURNING `+ticketColumns, limit, minAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("leasing case tickets for sync: %w", err)
	}
	return collectTickets(rows)
}

// RecordSync stores a sync outcome. A failed sync (syncErr set) keeps the
// last status read and shows the error class on the case.
func (s *PgTicketStore) RecordSync(
	ctx context.Context, id int64, st IssueStatus, syncErr string,
) error {
	qctx, cancel := s.query(ctx)
	defer cancel()
	_, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.case_tickets SET
		    status = CASE WHEN $4 = '' THEN left($2, 200) ELSE status END,
		    status_category = CASE WHEN $4 = '' THEN left($3, 40) ELSE status_category END,
		    sync_error = $4, last_synced_at = now()
		WHERE id = $1`, id, st.Name, st.Category, syncErr)
	if err != nil {
		return fmt.Errorf("recording sync of ticket %d: %w", id, err)
	}
	return nil
}

// ClaimResolvedComment marks the resolve comment as made; false when it
// already was.
func (s *PgTicketStore) ClaimResolvedComment(ctx context.Context, id int64) (bool, error) {
	qctx, cancel := s.query(ctx)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.case_tickets
		SET resolved_comment_at = now()
		WHERE id = $1 AND resolved_comment_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("claiming the resolve comment of ticket %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseResolvedComment clears a claim whose comment failed.
func (s *PgTicketStore) ReleaseResolvedComment(ctx context.Context, id int64) error {
	qctx, cancel := s.query(ctx)
	defer cancel()
	_, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.case_tickets
		SET resolved_comment_at = NULL WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("releasing the resolve comment of ticket %d: %w", id, err)
	}
	return nil
}
