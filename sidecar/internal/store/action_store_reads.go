package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ListPending returns pending actions, optionally filtered
// by database.
func (s *ActionStore) ListPending(
	ctx context.Context, databaseID *int,
) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	query := listPendingBaseSQL
	if databaseID != nil {
		query += " AND q.database_id = $1"
		r, err := s.pool.Query(qctx, query, *databaseID)
		if err != nil {
			return nil, fmt.Errorf("listing pending actions: %w", err)
		}
		defer r.Close()
		return scanQueuedActions(r)
	}

	r, err := s.pool.Query(qctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing pending actions: %w", err)
	}
	defer r.Close()
	return scanQueuedActions(r)
}

// ListPendingByFinding returns all pending (non-expired) queued
// actions for the given finding_id. Used by the inline action flow
// on the Findings page so a user can approve/reject without
// hopping to the Actions page.
func (s *ActionStore) ListPendingByFinding(
	ctx context.Context, findingID int,
) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	r, err := s.pool.Query(qctx,
		listPendingBaseSQL+" AND q.finding_id = $1", findingID)
	if err != nil {
		return nil, fmt.Errorf(
			"listing pending by finding %d: %w", findingID, err)
	}
	defer r.Close()
	return scanQueuedActions(r)
}

// ListLedgerByFinding returns non-executed queued actions for the given
// finding_id, including expired, failed, blocked, and rejected proposals.
func (s *ActionStore) ListLedgerByFinding(
	ctx context.Context, findingID int,
) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	r, err := s.pool.Query(qctx,
		queuedActionSelectSQL+`
 WHERE q.finding_id = $1
   AND q.status <> 'executed'
 ORDER BY q.proposed_at DESC, q.id DESC
 LIMIT 20`, findingID)
	if err != nil {
		return nil, fmt.Errorf(
			"listing action ledger by finding %d: %w", findingID, err)
	}
	defer r.Close()
	return scanQueuedActions(r)
}

// ListLedgerByFindingIDs returns, per finding, the newest 20 non-executed
// queued actions, newest first. An empty input sends no query.
func (s *ActionStore) ListLedgerByFindingIDs(
	ctx context.Context, findingIDs []int,
) (map[int][]QueuedAction, error) {
	if len(findingIDs) == 0 {
		return make(map[int][]QueuedAction), nil
	}
	actions, err := s.queryLedgerRows(ctx, findingIDs)
	if err != nil {
		return nil, err
	}
	return groupLedgerByFinding(actions, len(findingIDs)), nil
}

// ledgerByFindingsSQL ranks each finding's non-executed queue rows and keeps
// the newest 20. The ranked subquery is aliased q so it reads the same
// queuedActionColumns as every other queue read.
const ledgerByFindingsSQL = `/* pg_sage */SELECT ` + queuedActionColumns + `
 FROM (
    SELECT q.*, row_number() OVER (
               PARTITION BY q.finding_id
               ORDER BY q.proposed_at DESC, q.id DESC
           ) AS ledger_rank
      FROM sage.action_queue q
     WHERE q.finding_id = ANY($1)
       AND q.status <> 'executed'
 ) q
 WHERE q.ledger_rank <= 20
 ORDER BY q.finding_id, q.proposed_at DESC, q.id DESC`

func (s *ActionStore) queryLedgerRows(
	ctx context.Context, findingIDs []int,
) ([]QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := s.pool.Query(qctx, ledgerByFindingsSQL, findingIDs)
	if err != nil {
		return nil, fmt.Errorf("listing action ledger by findings: %w", err)
	}
	defer r.Close()
	return scanQueuedActions(r)
}

// groupLedgerByFinding groups queue rows by finding, keeping their order.
func groupLedgerByFinding(actions []QueuedAction, findings int) map[int][]QueuedAction {
	out := make(map[int][]QueuedAction, findings)
	for _, action := range actions {
		out[action.FindingID] = append(out[action.FindingID], action)
	}
	return out
}

// queuedActionColumns are the action_queue columns scanQueuedAction reads,
// in order, from the queue aliased q.
const queuedActionColumns = `q.id, q.database_id, q.finding_id,
 q.proposed_sql, q.rollback_sql, q.action_risk, q.status,
 q.proposed_at, q.decided_by, q.decided_at, q.expires_at,
 COALESCE(q.reason, ''), COALESCE(q.action_type, ''),
 COALESCE(q.identity_key, ''), COALESCE(q.policy_decision, ''),
 COALESCE(q.guardrails, '[]'::jsonb), COALESCE(q.attempt_count, 0),
 q.last_attempt_at, q.cooldown_until,
 COALESCE(q.failure_fingerprint, ''),
 COALESCE(q.last_failure_fingerprint, ''),
 COALESCE(q.verification_status, ''),
 COALESCE(q.shadow_toil_minutes, 0), q.action_log_id,
 q.recommendation_id, q.recommendation_revision, COALESCE(q.content_hash, ''),
 COALESCE(q.proposed_via, ''), COALESCE(q.proposed_by, ''),
 COALESCE(q.principal_id, ''), q.reverts_action_id,
 q.scheduled_by, q.scheduled_at, q.run_after, q.window_closes_at`

// listPendingBaseSQL lists pending items of open findings. A revert item
// (reverts_action_id) is listed whatever its finding's state: the finding
// was acted on by the action it reverts (policy-05).
const listPendingBaseSQL = `/* pg_sage */SELECT ` + queuedActionColumns + `
 FROM sage.action_queue q
 LEFT JOIN sage.findings f ON f.id = q.finding_id
WHERE q.status = 'pending'
   AND q.expires_at > now()
   AND (q.cooldown_until IS NULL OR q.cooldown_until <= now())
   AND (q.reverts_action_id IS NOT NULL
        OR (f.status = 'open' AND f.acted_on_at IS NULL AND f.resolved_at IS NULL))`

const queuedActionSelectSQL = `/* pg_sage */SELECT ` + queuedActionColumns + `
 FROM sage.action_queue q`

func (s *ActionStore) FindingEvidencePresent(
	ctx context.Context,
	findingID int,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT 1
		   FROM sage.findings
		  WHERE id = $1
		    AND status = 'open'
		    AND acted_on_at IS NULL
		    AND resolved_at IS NULL
		  LIMIT 1`,
		findingID,
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf(
			"checking finding evidence %d: %w", findingID, err)
	}
	return true, nil
}

// GetByID returns a single queued action.
func (s *ActionStore) GetByID(
	ctx context.Context, id int,
) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	a, err := scanQueuedAction(s.pool.QueryRow(qctx,
		queuedActionSelectSQL+` WHERE q.id = $1`, id))
	if err != nil {
		return nil, fmt.Errorf("getting action %d: %w", id, err)
	}
	return &a, nil
}

// PendingCount returns the number of pending (non-expired) actions.
func (s *ActionStore) PendingCount(
	ctx context.Context,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var n int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT COUNT(*)
		   FROM sage.action_queue q
		   JOIN sage.findings f ON f.id = q.finding_id
		  WHERE q.status = 'pending'
		    AND q.expires_at > now()
		    AND f.status = 'open'
		    AND f.acted_on_at IS NULL
		    AND f.resolved_at IS NULL`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting pending actions: %w", err)
	}
	return n, nil
}

// HasPendingForFinding checks if a finding already has a pending
// action in the queue. An approval scheduled for its window counts.
func (s *ActionStore) HasPendingForFinding(
	ctx context.Context, findingID int,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT 1
		   FROM sage.action_queue q
		   JOIN sage.findings f ON f.id = q.finding_id
		  WHERE q.finding_id = $1
		    AND ((q.status = 'pending' AND q.expires_at > now())
		         OR q.status = 'scheduled')
		    AND f.status = 'open'
		    AND f.acted_on_at IS NULL
		    AND f.resolved_at IS NULL
		 LIMIT 1`, findingID,
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf(
			"checking pending action for finding %d: %w",
			findingID, err)
	}
	return true, nil
}

// HasPendingForSQL checks whether the same SQL statement is already pending.
func (s *ActionStore) HasPendingForSQL(
	ctx context.Context, sql string,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT 1
		   FROM sage.action_queue q
		   JOIN sage.findings f ON f.id = q.finding_id
		  WHERE q.proposed_sql = $1
		    AND ((q.status = 'pending' AND q.expires_at > now())
		         OR q.status = 'scheduled')
		    AND f.status = 'open'
		    AND f.acted_on_at IS NULL
		    AND f.resolved_at IS NULL
		 LIMIT 1`, sql,
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf(
			"checking pending action for SQL: %w", err)
	}
	return true, nil
}

// HasRecentlyRejectedForFinding checks whether the same finding had a
// rejected proposal inside the cooldown window.
func (s *ActionStore) HasRecentlyRejectedForFinding(
	ctx context.Context, findingID int, cooldown time.Duration,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT 1 FROM sage.action_queue
		 WHERE finding_id = $1
		   AND status = 'rejected'
		   AND decided_at > now() - ($2::text)::interval
		 LIMIT 1`,
		findingID, durationInterval(cooldown),
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf(
			"checking rejected action for finding %d: %w",
			findingID, err)
	}
	return true, nil
}

// HasRecentlyRejectedForSQL checks whether identical SQL had a rejected
// proposal inside the cooldown window.
func (s *ActionStore) HasRecentlyRejectedForSQL(
	ctx context.Context, sql string, cooldown time.Duration,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT 1 FROM sage.action_queue
		 WHERE proposed_sql = $1
		   AND status = 'rejected'
		   AND decided_at > now() - ($2::text)::interval
		 LIMIT 1`,
		sql, durationInterval(cooldown),
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf(
			"checking rejected action for SQL: %w", err)
	}
	return true, nil
}

func durationInterval(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	return fmt.Sprintf("%f seconds", d.Seconds())
}
