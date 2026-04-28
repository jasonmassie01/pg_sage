package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QueuedAction represents a row from sage.action_queue.
type QueuedAction struct {
	ID                     int
	DatabaseID             *int
	FindingID              int
	ProposedSQL            string
	RollbackSQL            string
	ActionRisk             string
	Status                 string // pending, approved, rejected, expired
	ProposedAt             time.Time
	DecidedBy              *int
	DecidedAt              *time.Time
	ExpiresAt              time.Time
	Reason                 string
	ActionType             string
	IdentityKey            string
	PolicyDecision         string
	Guardrails             []string
	AttemptCount           int
	LastAttemptAt          *time.Time
	CooldownUntil          *time.Time
	FailureFingerprint     string
	LastFailureFingerprint string
	VerificationStatus     string
	ShadowToilMinutes      int
	ActionLogID            *int64
}

type ActionProposalMetadata struct {
	ActionType         string
	IdentityKey        string
	PolicyDecision     string
	Guardrails         []string
	VerificationStatus string
	ShadowToilMinutes  int
	ExpiresAt          *time.Time
}

// ActionStore handles CRUD for sage.action_queue.
type ActionStore struct {
	pool *pgxpool.Pool
}

// NewActionStore creates an ActionStore.
func NewActionStore(pool *pgxpool.Pool) *ActionStore {
	return &ActionStore{pool: pool}
}

// Propose adds an action to the queue. Returns the queue ID.
func (s *ActionStore) Propose(
	ctx context.Context,
	databaseID *int, findingID int,
	sql, rollbackSQL, risk string,
) (int, error) {
	return s.ProposeWithMetadata(
		ctx, databaseID, findingID, sql, rollbackSQL, risk,
		ActionProposalMetadata{},
	)
}

func (s *ActionStore) ProposeWithMetadata(
	ctx context.Context,
	databaseID *int, findingID int,
	sql, rollbackSQL, risk string,
	meta ActionProposalMetadata,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var id int
	guardrails, err := json.Marshal(meta.Guardrails)
	if err != nil {
		return 0, fmt.Errorf("encoding action guardrails: %w", err)
	}
	err = s.pool.QueryRow(qctx,
		`INSERT INTO sage.action_queue
		    (database_id, finding_id, proposed_sql,
		     rollback_sql, action_risk, action_type,
		     identity_key, policy_decision, guardrails,
		     verification_status, shadow_toil_minutes, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		         $9::jsonb, COALESCE($10, 'not_started'),
		         $11, COALESCE($12, now() + INTERVAL '7 days'))
		 RETURNING id`,
		databaseID, findingID, sql,
		NilIfEmpty(rollbackSQL), risk,
		NilIfEmpty(meta.ActionType), NilIfEmpty(meta.IdentityKey),
		NilIfEmpty(meta.PolicyDecision), guardrails,
		NilIfEmpty(meta.VerificationStatus), meta.ShadowToilMinutes,
		meta.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("proposing action: %w", err)
	}
	return id, nil
}

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

const listPendingBaseSQL = `SELECT q.id, q.database_id, q.finding_id,
 q.proposed_sql, q.rollback_sql, q.action_risk, q.status,
 q.proposed_at, q.decided_by, q.decided_at, q.expires_at,
 COALESCE(q.reason, ''), COALESCE(q.action_type, ''),
 COALESCE(q.identity_key, ''), COALESCE(q.policy_decision, ''),
 COALESCE(q.guardrails, '[]'::jsonb), COALESCE(q.attempt_count, 0),
 q.last_attempt_at, q.cooldown_until,
 COALESCE(q.failure_fingerprint, ''),
 COALESCE(q.last_failure_fingerprint, ''),
 COALESCE(q.verification_status, ''),
 COALESCE(q.shadow_toil_minutes, 0), q.action_log_id
 FROM sage.action_queue q
 JOIN sage.findings f ON f.id = q.finding_id
WHERE q.status = 'pending'
   AND q.expires_at > now()
   AND (q.cooldown_until IS NULL OR q.cooldown_until <= now())
   AND f.status = 'open'
   AND f.acted_on_at IS NULL
   AND f.resolved_at IS NULL`

// Approve marks an action as approved. Returns the action.
func (s *ActionStore) Approve(
	ctx context.Context, queueID, userID int,
) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var a QueuedAction
	var rollback *string
	var guardrails []byte
	err := s.pool.QueryRow(qctx,
		`UPDATE sage.action_queue q
		 SET status = 'approved',
		     decided_by = $1,
		     decided_at = now()
	 WHERE q.id = $2
	   AND q.status = 'pending'
	   AND q.expires_at > now()
	   AND (q.cooldown_until IS NULL OR q.cooldown_until <= now())
	   AND EXISTS (
		       SELECT 1 FROM sage.findings f
		        WHERE f.id = q.finding_id
		          AND f.status = 'open'
		          AND f.acted_on_at IS NULL
		          AND f.resolved_at IS NULL
		   )
	 RETURNING q.id, q.database_id, q.finding_id,
	     q.proposed_sql, q.rollback_sql, q.action_risk,
	     q.status, q.proposed_at, q.decided_by, q.decided_at,
	     q.expires_at, COALESCE(q.reason, ''),
	     COALESCE(q.action_type, ''), COALESCE(q.identity_key, ''),
	     COALESCE(q.policy_decision, ''),
	     COALESCE(q.guardrails, '[]'::jsonb),
	     COALESCE(q.attempt_count, 0), q.last_attempt_at,
	     q.cooldown_until, COALESCE(q.failure_fingerprint, ''),
	     COALESCE(q.last_failure_fingerprint, ''),
	     COALESCE(q.verification_status, ''),
	     COALESCE(q.shadow_toil_minutes, 0), q.action_log_id`,
		userID, queueID,
	).Scan(
		&a.ID, &a.DatabaseID, &a.FindingID,
		&a.ProposedSQL, &rollback, &a.ActionRisk,
		&a.Status, &a.ProposedAt, &a.DecidedBy,
		&a.DecidedAt, &a.ExpiresAt, &a.Reason,
		&a.ActionType, &a.IdentityKey, &a.PolicyDecision,
		&guardrails, &a.AttemptCount, &a.LastAttemptAt,
		&a.CooldownUntil, &a.FailureFingerprint,
		&a.LastFailureFingerprint, &a.VerificationStatus,
		&a.ShadowToilMinutes, &a.ActionLogID,
	)
	if err != nil {
		return nil, fmt.Errorf("approving action %d: %w", queueID, err)
	}
	if rollback != nil {
		a.RollbackSQL = *rollback
	}
	a.Guardrails = decodeGuardrails(guardrails)
	return &a, nil
}

// Reject marks an action as rejected with a reason.
func (s *ActionStore) Reject(
	ctx context.Context, queueID, userID int, reason string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.action_queue
		 SET status = 'rejected',
		     decided_by = $1,
		     decided_at = now(),
		     reason = $2
		 WHERE id = $3 AND status = 'pending'`,
		userID, reason, queueID,
	)
	if err != nil {
		return fmt.Errorf("rejecting action %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found or not pending", queueID)
	}
	return nil
}

func (s *ActionStore) MarkAttemptFailed(
	ctx context.Context,
	queueID int,
	failure string,
	cooldown time.Duration,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cooldownUntil := time.Now().UTC().Add(cooldown)
	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.action_queue
		 SET status = 'failed',
		     attempt_count = attempt_count + 1,
		     last_attempt_at = now(),
		     cooldown_until = CASE
		         WHEN $2 THEN $3::timestamptz ELSE NULL::timestamptz END,
		     last_failure_fingerprint = failure_fingerprint,
		     failure_fingerprint = $4,
		     verification_status = 'failed',
		     reason = $5
		 WHERE id = $1`,
		queueID, cooldown > 0, cooldownUntil,
		failureFingerprint(failure), failure,
	)
	if err != nil {
		return fmt.Errorf("marking action %d failed: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found", queueID)
	}
	return nil
}

func (s *ActionStore) MarkExecuted(
	ctx context.Context,
	queueID int,
	actionLogID int64,
	verificationStatus string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if verificationStatus == "" {
		verificationStatus = "verified"
	}
	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.action_queue
		 SET status = 'executed',
		     action_log_id = $2,
		     verification_status = $3,
		     last_attempt_at = now(),
		     cooldown_until = NULL
		 WHERE id = $1`,
		queueID, actionLogID, verificationStatus,
	)
	if err != nil {
		return fmt.Errorf("marking action %d executed: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found", queueID)
	}
	return nil
}

// ExpireStale marks expired pending actions.
func (s *ActionStore) ExpireStale(
	ctx context.Context,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.action_queue
		 SET status = 'expired'
		 WHERE status = 'pending'
		   AND expires_at <= now()`,
	)
	if err != nil {
		return 0, fmt.Errorf("expiring stale actions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkExpiredByReadiness records proposals that are no longer actionable
// because their deterministic lifecycle check has closed the gate.
func (s *ActionStore) MarkExpiredByReadiness(
	ctx context.Context,
) (int, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.action_queue
		 SET status = 'expired',
		     reason = CASE
		         WHEN expires_at <= now()
		         THEN 'action proposal expired'
		         ELSE reason
		     END
		 WHERE status IN ('pending', 'failed')
		   AND expires_at <= now()`,
	)
	if err != nil {
		return 0, fmt.Errorf("marking readiness-expired actions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// GetByID returns a single queued action.
func (s *ActionStore) GetByID(
	ctx context.Context, id int,
) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var a QueuedAction
	var rollback *string
	var guardrails []byte
	err := s.pool.QueryRow(qctx,
		`SELECT id, database_id, finding_id,
		     proposed_sql, rollback_sql, action_risk,
		     status, proposed_at, decided_by, decided_at,
		     expires_at, COALESCE(reason, ''),
		     COALESCE(action_type, ''), COALESCE(identity_key, ''),
		     COALESCE(policy_decision, ''),
		     COALESCE(guardrails, '[]'::jsonb),
		     COALESCE(attempt_count, 0), last_attempt_at,
		     cooldown_until, COALESCE(failure_fingerprint, ''),
	     COALESCE(last_failure_fingerprint, ''),
	     COALESCE(verification_status, ''),
	     COALESCE(shadow_toil_minutes, 0), action_log_id
	 FROM sage.action_queue WHERE id = $1`, id,
	).Scan(
		&a.ID, &a.DatabaseID, &a.FindingID,
		&a.ProposedSQL, &rollback, &a.ActionRisk,
		&a.Status, &a.ProposedAt, &a.DecidedBy,
		&a.DecidedAt, &a.ExpiresAt, &a.Reason,
		&a.ActionType, &a.IdentityKey, &a.PolicyDecision,
		&guardrails, &a.AttemptCount, &a.LastAttemptAt,
		&a.CooldownUntil, &a.FailureFingerprint,
		&a.LastFailureFingerprint, &a.VerificationStatus,
		&a.ShadowToilMinutes, &a.ActionLogID,
	)
	if err != nil {
		return nil, fmt.Errorf("getting action %d: %w", id, err)
	}
	if rollback != nil {
		a.RollbackSQL = *rollback
	}
	a.Guardrails = decodeGuardrails(guardrails)
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
		`SELECT COUNT(*)
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
// action in the queue.
func (s *ActionStore) HasPendingForFinding(
	ctx context.Context, findingID int,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := s.pool.QueryRow(qctx,
		`SELECT 1
		   FROM sage.action_queue q
		   JOIN sage.findings f ON f.id = q.finding_id
		  WHERE q.finding_id = $1
		    AND q.status = 'pending'
		    AND q.expires_at > now()
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
		`SELECT 1
		   FROM sage.action_queue q
		   JOIN sage.findings f ON f.id = q.finding_id
		  WHERE q.proposed_sql = $1
		    AND q.status = 'pending'
		    AND q.expires_at > now()
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
		`SELECT 1 FROM sage.action_queue
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
		`SELECT 1 FROM sage.action_queue
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

// NilIfEmpty returns nil for empty strings.
func NilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func durationInterval(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	return fmt.Sprintf("%f seconds", d.Seconds())
}
