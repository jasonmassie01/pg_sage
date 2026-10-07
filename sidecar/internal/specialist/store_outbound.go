package specialist

import (
	"context"
	"fmt"
	"time"
)

// ClaimOutbound leases up to limit due result posts: their next attempt
// moves past the lease, so a concurrent claimer (another sidecar) skips
// them.
func (s *PGStore) ClaimOutbound(ctx context.Context, now time.Time, lease time.Duration,
	limit int) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ WITH due AS (
			SELECT id FROM sage.specialist_requests
			WHERE outbound = 'pending' AND outbound_next_at <= $1
			ORDER BY outbound_next_at LIMIT $3
			FOR UPDATE SKIP LOCKED)
		UPDATE sage.specialist_requests r
		SET outbound_next_at = $1::timestamptz + make_interval(secs => $2),
		    updated_at = now()
		FROM due WHERE r.id = due.id
		RETURNING `+qualifiedRecordColumns, now, lease.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim result posts: %w", err)
	}
	return scanRecords(rows)
}

// FinishOutbound records one attempt's outcome: its state, error and the
// next attempt.
func (s *PGStore) FinishOutbound(ctx context.Context, id, state, errText string,
	next time.Time) error {
	_, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.specialist_requests
		SET outbound = $2, outbound_error = left($3, 2000), outbound_next_at = $4,
		    outbound_attempts = outbound_attempts + 1, updated_at = now()
		WHERE id = $1::uuid`, id, state, errText, next)
	if err != nil {
		return fmt.Errorf("record result post %s: %w", id, err)
	}
	return nil
}

// RescheduleOutbound moves a pending post (its investigation still runs)
// without counting an attempt.
func (s *PGStore) RescheduleOutbound(ctx context.Context, id string, next time.Time) error {
	_, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.specialist_requests
		SET outbound_next_at = $2, updated_at = now() WHERE id = $1::uuid`, id, next)
	if err != nil {
		return fmt.Errorf("reschedule result post %s: %w", id, err)
	}
	return nil
}

// qualifiedRecordColumns are recordColumns with the table alias r.
const qualifiedRecordColumns = `r.id::text, r.kind, r.token_id, r.identity_name, r.actor,
	r.transport, r.database_name, COALESCE(r.investigation_id, ''), r.created, r.match,
	r.symptom, r.time_window, r.external_ref, r.remediation_id, r.verdict, r.reason,
	r.outbound, r.outbound_attempts, r.outbound_next_at, r.outbound_error, r.created_at,
	r.query_scope`
