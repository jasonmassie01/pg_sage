package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The approval handoff writes into the existing approval flow of the
// monitored database (Codex §7: one outbox row per proposal, deduplicated
// by its identity key): an open sage.findings row (category sre_action)
// anchors one sage.action_queue item. The finding has no recommended SQL,
// so the manual "take action" path can never run it; only an approval of
// the queue item does, through the action service.

// ApprovalIdentityPrefix keys a proposal's approval item.
const ApprovalIdentityPrefix = "sre_proposal:"

// ProposalIDFromIdentityKey parses an approval item's identity key.
func ProposalIDFromIdentityKey(key string) (sre.UUID, bool) {
	raw, ok := strings.CutPrefix(key, ApprovalIdentityPrefix)
	if !ok {
		return "", false
	}
	id, err := sre.ParseUUID(raw)
	return id, err == nil
}

// ApprovalItem is one proposal's approval request.
type ApprovalItem struct {
	ProposalID      sre.UUID
	InvestigationID sre.UUID
	PID             int32
	SQL             string
	Title           string
	Detail          string
	Database        string
	ExpiresAt       time.Time
}

// QueuedItem is a proposal's approval item; Created is false when it
// already existed.
type QueuedItem struct {
	QueueID   int
	FindingID int
	Created   bool
}

// QueueStatus is an approval item's state. An unexpired pending item past
// its expiry reads as expired.
type QueueStatus struct {
	Status             string
	DecidedBy          int
	Reason             string
	VerificationStatus string
	ExpiresAt          time.Time
}

// ApprovalQueue is the existing approval flow, as the service uses it.
type ApprovalQueue interface {
	Enqueue(ctx context.Context, item ApprovalItem) (QueuedItem, error)
	Status(ctx context.Context, queueID int) (QueueStatus, error)
	SetVerification(ctx context.Context, queueID int, status string) error
	Resolve(ctx context.Context, queueID int) error
}

// PGApprovalQueue is the approval queue of one monitored database.
type PGApprovalQueue struct {
	pool       *pgxpool.Pool
	databaseID *int
}

// NewPGApprovalQueue binds the queue to a monitored database pool;
// databaseID is the control-plane id (nil outside meta-db mode).
func NewPGApprovalQueue(pool *pgxpool.Pool, databaseID *int) *PGApprovalQueue {
	return &PGApprovalQueue{pool: pool, databaseID: databaseID}
}

func (i ApprovalItem) validate() error {
	if _, err := sre.ParseUUID(string(i.ProposalID)); err != nil {
		return err
	}
	switch {
	case i.PID <= 0:
		return fmt.Errorf("%w: approval item without a pid", sre.ErrInvalidRequest)
	case i.SQL != fmt.Sprintf("SELECT pg_cancel_backend(%d)", i.PID):
		return fmt.Errorf("%w: approval SQL %q is not the cancel of pid %d",
			sre.ErrInvalidRequest, i.SQL, i.PID)
	case !i.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: approval item already expired", sre.ErrInvalidRequest)
	}
	return nil
}

// Enqueue creates the proposal's finding and approval item once; a second
// call (or a concurrent one) returns the same item.
func (q *PGApprovalQueue) Enqueue(ctx context.Context, item ApprovalItem) (QueuedItem,
	error) {
	if err := item.validate(); err != nil {
		return QueuedItem{}, err
	}
	key := ApprovalIdentityPrefix + string(item.ProposalID)
	var out QueuedItem
	err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			key); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id, COALESCE(finding_id, 0)
			FROM sage.action_queue WHERE identity_key = $1 ORDER BY id LIMIT 1`, key).
			Scan(&out.QueueID, &out.FindingID)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out.Created = true
		return q.insertItem(ctx, tx, key, item, &out)
	})
	if err != nil {
		return QueuedItem{}, fmt.Errorf("queueing approval of proposal %s: %w",
			item.ProposalID, err)
	}
	return out, nil
}

func (q *PGApprovalQueue) insertItem(ctx context.Context, tx pgx.Tx, key string,
	item ApprovalItem, out *QueuedItem) error {
	detail, err := json.Marshal(map[string]any{"proposal_id": string(item.ProposalID),
		"investigation_id": string(item.InvestigationID), "database": item.Database,
		"summary": item.Detail, "source": "sage_sre"})
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ('sre_action', 'critical', 'backend', $1, $2, $3::jsonb, $4)
		ON CONFLICT (category, object_identifier) WHERE status = 'open'
		DO UPDATE SET last_seen = now() RETURNING id`, key, item.Title, string(detail),
		"Approve or deny the proposed cancel from the Cases panel, the approval queue "+
			"or ChatOps.").Scan(&out.FindingID)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.action_queue
		(database_id, finding_id, proposed_sql, action_risk, action_type, identity_key,
		 policy_decision, guardrails, expires_at)
		VALUES ($1, $2, $3, 'moderate', 'cancel_backend', $4, 'queue_approval',
		        '["approval_required", "identity_recheck", "evidence_age_5s"]'::jsonb, $5)
		RETURNING id`, q.databaseID, out.FindingID, item.SQL, key, item.ExpiresAt).
		Scan(&out.QueueID)
}

// Status reads an approval item.
func (q *PGApprovalQueue) Status(ctx context.Context, queueID int) (QueueStatus, error) {
	var s QueueStatus
	err := q.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		CASE WHEN status = 'pending' AND expires_at <= now() THEN 'expired' ELSE status END,
		COALESCE(decided_by, 0), COALESCE(reason, ''), COALESCE(verification_status, ''),
		expires_at
		FROM sage.action_queue WHERE id = $1`, queueID).
		Scan(&s.Status, &s.DecidedBy, &s.Reason, &s.VerificationStatus, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, fmt.Errorf("%w: approval item %d", ErrProposalNotFound, queueID)
	}
	if err != nil {
		return s, fmt.Errorf("reading approval item %d: %w", queueID, err)
	}
	return s, nil
}

var queueVerifications = map[string]bool{"verified": true, "failed": true,
	"inconclusive": true}

// SetVerification records the recovery verdict on the approval item.
func (q *PGApprovalQueue) SetVerification(ctx context.Context, queueID int,
	status string) error {
	if !queueVerifications[status] {
		return fmt.Errorf("%w: verification status %q", sre.ErrInvalidRequest, status)
	}
	tag, err := q.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.action_queue
		SET verification_status = $2 WHERE id = $1`, queueID, status)
	if err != nil {
		return fmt.Errorf("recording verification of approval item %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: approval item %d", ErrProposalNotFound, queueID)
	}
	return nil
}

// Resolve closes the approval item's finding (denied, expired, refused or
// verified), so it does not linger open.
func (q *PGApprovalQueue) Resolve(ctx context.Context, queueID int) error {
	tag, err := q.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.findings f
		SET status = 'resolved', resolved_at = COALESCE(f.resolved_at, now())
		FROM sage.action_queue q WHERE q.id = $1 AND f.id = q.finding_id`, queueID)
	if err != nil {
		return fmt.Errorf("resolving approval item %d: %w", queueID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: approval item %d", ErrProposalNotFound, queueID)
	}
	return nil
}
