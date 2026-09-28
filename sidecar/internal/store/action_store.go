package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/recommendation"
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
	// RecommendationID, RecommendationRevision and ContentHash are the
	// recommendation revision this proposal was queued for (nil: legacy).
	RecommendationID       *int64
	RecommendationRevision *int
	ContentHash            string
}

type ActionProposalMetadata struct {
	ActionType         string
	IdentityKey        string
	PolicyDecision     string
	Guardrails         []string
	VerificationStatus string
	ShadowToilMinutes  int
	ExpiresAt          *time.Time
	// RecommendationID, RecommendationRevision and ContentHash pin the
	// queued proposal to one immutable recommendation revision (C04).
	RecommendationID       int64
	RecommendationRevision int
	ContentHash            string
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
		`/* pg_sage */ INSERT INTO sage.action_queue
		    (database_id, finding_id, proposed_sql,
		     rollback_sql, action_risk, action_type,
		     identity_key, policy_decision, guardrails,
		     verification_status, shadow_toil_minutes, expires_at,
		     recommendation_id, recommendation_revision, content_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		         $9::jsonb, COALESCE($10, 'not_started'),
		         $11, COALESCE($12, now() + INTERVAL '7 days'),
		         NULLIF($13::bigint, 0), NULLIF($14::int, 0), $15)
		 RETURNING id`,
		databaseID, findingID, sql,
		NilIfEmpty(rollbackSQL), risk,
		NilIfEmpty(meta.ActionType), NilIfEmpty(meta.IdentityKey),
		NilIfEmpty(meta.PolicyDecision), guardrails,
		NilIfEmpty(meta.VerificationStatus), meta.ShadowToilMinutes,
		meta.ExpiresAt, meta.RecommendationID, meta.RecommendationRevision,
		NilIfEmpty(meta.ContentHash),
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

func (s *ActionStore) ListLedgerByFindingIDs(
	ctx context.Context, findingIDs []int,
) (map[int][]QueuedAction, error) {
	out := make(map[int][]QueuedAction, len(findingIDs))
	if len(findingIDs) == 0 {
		return out, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	r, err := s.pool.Query(qctx, `/* pg_sage */ 
SELECT id, database_id, finding_id, proposed_sql, rollback_sql,
       action_risk, status, proposed_at, decided_by, decided_at,
       expires_at, reason, action_type, identity_key, policy_decision,
       guardrails, attempt_count, last_attempt_at, cooldown_until,
       failure_fingerprint, last_failure_fingerprint, verification_status,
       shadow_toil_minutes, action_log_id, recommendation_id,
       recommendation_revision, content_hash
FROM (
    SELECT q.id, q.database_id, q.finding_id, q.proposed_sql,
           q.rollback_sql, q.action_risk, q.status, q.proposed_at,
           q.decided_by, q.decided_at, q.expires_at,
           COALESCE(q.reason, '') AS reason,
           COALESCE(q.action_type, '') AS action_type,
           COALESCE(q.identity_key, '') AS identity_key,
           COALESCE(q.policy_decision, '') AS policy_decision,
           COALESCE(q.guardrails, '[]'::jsonb) AS guardrails,
           COALESCE(q.attempt_count, 0) AS attempt_count,
           q.last_attempt_at, q.cooldown_until,
           COALESCE(q.failure_fingerprint, '') AS failure_fingerprint,
           COALESCE(q.last_failure_fingerprint, '') AS last_failure_fingerprint,
           COALESCE(q.verification_status, '') AS verification_status,
           COALESCE(q.shadow_toil_minutes, 0) AS shadow_toil_minutes,
           q.action_log_id, q.recommendation_id, q.recommendation_revision,
           COALESCE(q.content_hash, '') AS content_hash,
           row_number() OVER (
               PARTITION BY q.finding_id
               ORDER BY q.proposed_at DESC, q.id DESC
           ) AS rn
    FROM sage.action_queue q
    WHERE q.finding_id = ANY($1)
      AND q.status <> 'executed'
) ranked
WHERE rn <= 20
ORDER BY finding_id, proposed_at DESC, id DESC`, findingIDs)
	if err != nil {
		return nil, fmt.Errorf("listing action ledger by findings: %w", err)
	}
	defer r.Close()
	actions, err := scanQueuedActions(r)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		out[action.FindingID] = append(out[action.FindingID], action)
	}
	return out, nil
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
 q.recommendation_id, q.recommendation_revision, COALESCE(q.content_hash, '')`

const listPendingBaseSQL = `/* pg_sage */SELECT ` + queuedActionColumns + `
 FROM sage.action_queue q
 JOIN sage.findings f ON f.id = q.finding_id
WHERE q.status = 'pending'
   AND q.expires_at > now()
   AND (q.cooldown_until IS NULL OR q.cooldown_until <= now())
   AND f.status = 'open'
   AND f.acted_on_at IS NULL
   AND f.resolved_at IS NULL`

const queuedActionSelectSQL = `/* pg_sage */SELECT ` + queuedActionColumns + `
 FROM sage.action_queue q`

// Approve marks an action as approved and returns it. A proposal queued
// for a recommendation revision approves exactly that revision in the same
// transaction; if the recommendation was revised since, nothing changes
// and the error wraps recommendation.ErrRevised (C04).
func (s *ActionStore) Approve(
	ctx context.Context, queueID, userID int,
) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var a QueuedAction
	err := pgx.BeginFunc(qctx, s.pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanQueuedAction(tx.QueryRow(qctx, approveQueuedSQL, userID, queueID))
		if errors.Is(err, pgx.ErrNoRows) {
			return refusedApproval(qctx, tx, queueID, err)
		}
		if err != nil || a.RecommendationID == nil {
			return err
		}
		_, err = recommendation.ApproveTx(qctx, tx, *a.RecommendationID, a.ContentHash,
			"user:"+strconv.Itoa(userID))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("approving action %d: %w", queueID, err)
	}
	return &a, nil
}

// refusedApproval explains a queue row Approve could not approve: a row
// superseded by a new recommendation revision needs re-approval of that
// revision (recommendation.ErrRevised).
func refusedApproval(ctx context.Context, tx pgx.Tx, queueID int, cause error) error {
	var status string
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT status FROM sage.action_queue
		WHERE id = $1`, queueID).Scan(&status); err == nil && status == "superseded" {
		return fmt.Errorf("%w: queued proposal %d was superseded", recommendation.ErrRevised,
			queueID)
	}
	return cause
}

// approveQueuedSQL approves one pending, unexpired queue row whose
// finding is still open.
const approveQueuedSQL = `/* pg_sage */ UPDATE sage.action_queue q
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
	 RETURNING ` + queuedActionColumns

// Reject marks an action as rejected with a reason.
func (s *ActionStore) Reject(
	ctx context.Context, queueID, userID int, reason string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
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
		`/* pg_sage */ UPDATE sage.action_queue
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
		`/* pg_sage */ UPDATE sage.action_queue
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
		`/* pg_sage */ UPDATE sage.action_queue
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
		`/* pg_sage */ UPDATE sage.action_queue
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

func (s *ActionStore) MarkReadinessOutcome(
	ctx context.Context,
	queueID int,
	status string,
	reason string,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.action_queue
		    SET status = $2,
		        reason = $3
		  WHERE id = $1
		    AND status <> 'executed'`,
		queueID, status, reason,
	)
	if err != nil {
		return fmt.Errorf(
			"marking readiness outcome for action %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %d not found or already executed", queueID)
	}
	return nil
}

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
// action in the queue.
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
		`/* pg_sage */ SELECT 1
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
