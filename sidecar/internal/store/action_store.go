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
	Status                 string // pending, scheduled, approved, rejected, expired, ...
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
	// ProposedVia and ProposedBy: how and by whom the item was proposed.
	ProposedVia string
	ProposedBy  string
	// PrincipalID is the agent whose request queued the item, or "".
	PrincipalID string
	// RevertsActionID is the action a "Revert action N" item reverts: an
	// automatic rollback the policy withheld (policy-05). nil otherwise.
	RevertsActionID *int64
	// ScheduledBy, ScheduledAt, RunAfter and WindowClosesAt describe an
	// approval scheduled for the next maintenance window (policy-04): who
	// approved it and when, when it may start and when that window closes
	// (nil: the window stays open past the planning scan). Nil otherwise.
	ScheduledBy    *int
	ScheduledAt    *time.Time
	RunAfter       *time.Time
	WindowClosesAt *time.Time
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
	// ProposedVia and ProposedBy record how the item was proposed
	// ("ask_sage") and by whom; empty for pg_sage's own proposals.
	ProposedVia string
	ProposedBy  string
	// PrincipalID is the agent whose request queued the item ("" for
	// pg_sage's own and people's); its approved run is that agent's.
	PrincipalID string
	// RevertsActionID names the action a revert item reverts (0: none).
	RevertsActionID int64
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
		     recommendation_id, recommendation_revision, content_hash,
		     proposed_via, proposed_by, principal_id, reverts_action_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		         $9::jsonb, COALESCE($10, 'not_started'),
		         $11, COALESCE($12, now() + INTERVAL '7 days'),
		         NULLIF($13::bigint, 0), NULLIF($14::int, 0), $15, $16, $17, $18,
		         NULLIF($19::bigint, 0))
		 RETURNING id`,
		databaseID, findingID, sql,
		NilIfEmpty(rollbackSQL), risk,
		NilIfEmpty(meta.ActionType), NilIfEmpty(meta.IdentityKey),
		NilIfEmpty(meta.PolicyDecision), guardrails,
		NilIfEmpty(meta.VerificationStatus), meta.ShadowToilMinutes,
		meta.ExpiresAt, meta.RecommendationID, meta.RecommendationRevision,
		NilIfEmpty(meta.ContentHash), NilIfEmpty(meta.ProposedVia),
		NilIfEmpty(meta.ProposedBy), NilIfEmpty(meta.PrincipalID), meta.RevertsActionID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("proposing action: %w", err)
	}
	return id, nil
}

// Approve marks an action as approved and returns it. A proposal queued
// for a recommendation revision approves exactly that revision in the same
// transaction; if the recommendation was revised since, nothing changes
// and the error wraps recommendation.ErrRevised (C04).
func (s *ActionStore) Approve(
	ctx context.Context, queueID, userID int,
) (*QueuedAction, error) {
	return s.approve(ctx, queueID, userID, nil)
}

// approve approves a pending queue row; a non-nil expectSQL also requires
// its proposed SQL to be exactly that text (an approval card's SQL).
func (s *ActionStore) approve(
	ctx context.Context, queueID, userID int, expectSQL *string,
) (*QueuedAction, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var a QueuedAction
	err := pgx.BeginFunc(qctx, s.pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanQueuedAction(tx.QueryRow(qctx, approveQueuedSQL, userID, queueID,
			expectSQL))
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
	   AND ($3::text IS NULL OR q.proposed_sql = $3)
	   AND EXISTS (
	       SELECT 1 FROM sage.findings f
	        WHERE f.id = q.finding_id
	          AND f.status = 'open'
	          AND f.acted_on_at IS NULL
	          AND f.resolved_at IS NULL
	   )
	 RETURNING ` + queuedActionColumns

// NilIfEmpty returns nil for empty strings.
func NilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
