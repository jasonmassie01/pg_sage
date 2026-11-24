package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// Two-person principal changes (§6.11, agentgov-06). Outside
// single-operator mode a widening patch is recorded in
// sage.guard_principal_changes as pending; a second admin, who is neither
// the requester nor the agent's sponsor (current or incoming), confirms
// the identical patch within the TTL and it applies. A newer request
// supersedes the pending one; at most one is pending per principal. Both
// steps lock the principal row first, then the change row, so concurrent
// requests and confirms serialize.

// Change errors, distinguishable with errors.Is.
var (
	// ErrChangeNotFound is no such change for the principal (404).
	ErrChangeNotFound = errors.New("agentguard: principal change not found")
	// ErrChangeNotPending is a change already applied, superseded or
	// expired (409 change_not_pending).
	ErrChangeNotPending = errors.New("agentguard: the principal change is no longer pending")
	// ErrChangeExpired is a confirm after the TTL (409 change_expired);
	// the change is marked expired.
	ErrChangeExpired = errors.New("agentguard: the principal change expired; request it again")
	// ErrChangeMismatch is a confirm whose patch differs from the
	// request's (409 change_mismatch).
	ErrChangeMismatch = errors.New(
		"agentguard: the confirmed patch differs from the requested one")
	// ErrChangeWidens is a patch judged narrowing on an earlier read that
	// widens the principal as it now stands (409 agent_changed).
	ErrChangeWidens = errors.New("agentguard: the agent changed since it was read and " +
		"this patch now widens it; read the agent again and resend")
	// ErrSecondAdminRequired is the requester confirming their own change
	// (403 second_admin_required).
	ErrSecondAdminRequired = errors.New(
		"agentguard: a second admin must confirm; the requester cannot")
)

// PatchDoc is a patch as stored and served: only the fields it changes.
type PatchDoc struct {
	SponsorUserID *int    `json:"sponsor_user_id,omitempty"`
	Profile       *string `json:"profile,omitempty"`
	EnvCeiling    *Env    `json:"env_ceiling,omitempty"`
}

func (patch Patch) doc() PatchDoc {
	return PatchDoc(patch)
}

func (d PatchDoc) patch() Patch {
	return Patch(d)
}

func sameField[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// equal reports whether two patches change the same fields to the same
// values.
func (patch Patch) equal(o Patch) bool {
	return sameField(patch.SponsorUserID, o.SponsorUserID) &&
		sameField(patch.Profile, o.Profile) && sameField(patch.EnvCeiling, o.EnvCeiling)
}

// ChangeRequest is the first admin's widening patch. Audit.ActorUserID is
// the signed-in admin; Audit.Reason is kept with the change.
type ChangeRequest struct {
	PrincipalID string
	Patch       Patch
	Actor       string // the admin's email, as approvals record it
	TTL         time.Duration
	Audit       Audit
}

// ChangeConfirm is the second admin's confirm of change ChangeID.
type ChangeConfirm struct {
	PrincipalID string
	ChangeID    int64
	Patch       Patch
	Actor       string
	Audit       Audit
}

// PendingChange is one row of sage.guard_principal_changes.
type PendingChange struct {
	ID              int64     `json:"change_id"`
	PrincipalID     string    `json:"principal_id"`
	Patch           PatchDoc  `json:"patch"`
	Status          string    `json:"status"`
	RequestedBy     string    `json:"requested_by"`
	RequestedByUser int       `json:"requested_by_user"`
	Reason          string    `json:"reason"`
	RequestedAt     time.Time `json:"requested_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	SupersededID    int64     `json:"superseded_change_id,omitempty"`
}

func checkChangeShape(id string, patch Patch, actor string, a Audit, event string) error {
	if err := patch.validate(); err != nil {
		return err
	}
	if patch.empty() {
		return invalid("a change must set sponsor_user_id, profile or env_ceiling")
	}
	if err := a.validate(event); err != nil {
		return err
	}
	if a.ActorUserID <= 0 {
		return invalid("a signed-in admin must make a principal change")
	}
	if err := checkText("actor", actor, 1, maxActorLen); err != nil {
		return err
	}
	if !ValidID(id) {
		return fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	return nil
}

// RequestChange records req as the principal's pending change,
// superseding (or expiring) the one pending before, and writes
// agent_change_requested in the same transaction.
func (s *Store) RequestChange(ctx context.Context, req ChangeRequest) (PendingChange, error) {
	if req.TTL <= 0 {
		return PendingChange{}, invalid("the change TTL must be positive")
	}
	err := checkChangeShape(req.PrincipalID, req.Patch, req.Actor, req.Audit,
		auth.AuditAgentChangeRequested)
	if err != nil {
		return PendingChange{}, err
	}
	if err := s.ready(); err != nil {
		return PendingChange{}, err
	}
	var pc PendingChange
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var txErr error
		pc, txErr = requestInTx(ctx, tx, req)
		return txErr
	})
	return pc, err
}

func requestInTx(ctx context.Context, tx pgx.Tx, req ChangeRequest) (PendingChange, error) {
	id := req.PrincipalID
	cur, err := lockForChange(ctx, tx, id)
	if err != nil {
		return PendingChange{}, err
	}
	if err := checkSponsorMove(ctx, tx, id, cur.shape, req.Patch); err != nil {
		return PendingChange{}, err
	}
	pc := PendingChange{PrincipalID: id, Patch: req.Patch.doc(), Status: "pending",
		RequestedBy: req.Actor, RequestedByUser: req.Audit.ActorUserID,
		Reason: req.Audit.Reason}
	err = tx.QueryRow(ctx, `/* pg_sage guard_principal_change_close v1 */
		UPDATE sage.guard_principal_changes
		SET status = CASE WHEN expires_at <= now() THEN 'expired' ELSE 'superseded' END,
			decided_at = now()
		WHERE principal_id = $1 AND status = 'pending' RETURNING id`, id).
		Scan(&pc.SupersededID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PendingChange{}, fmt.Errorf("agentguard: closing the pending change of %s: %w",
			id, err)
	}
	doc, err := json.Marshal(pc.Patch)
	if err != nil {
		return PendingChange{}, fmt.Errorf("agentguard: encoding the change of %s: %w", id, err)
	}
	err = tx.QueryRow(ctx, `/* pg_sage guard_principal_change_request v1 */
		INSERT INTO sage.guard_principal_changes (principal_id, patch, requested_by,
			requested_by_user, reason, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
		RETURNING id, requested_at, expires_at`, id, doc, req.Actor, req.Audit.ActorUserID,
		req.Audit.Reason, req.TTL.Seconds()).Scan(&pc.ID, &pc.RequestedAt, &pc.ExpiresAt)
	if err != nil {
		return PendingChange{}, fmt.Errorf("agentguard: recording the change of %s: %w", id, err)
	}
	return pc, record(ctx, tx, req.Audit, auth.AuditAgentChangeRequested,
		cur.shape.SponsorUserID, map[string]any{"principal_id": id, "name": cur.name,
			"change_id": pc.ID, "before": cur.shape, "after": req.Patch.apply(cur.shape),
			"reason": req.Audit.Reason, "expires_at": pc.ExpiresAt,
			"superseded_change_id": pc.SupersededID})
}

// PendingChanges lists the principal's pending change (at most one),
// including one past its expiry that nothing has closed yet.
func (s *Store) PendingChanges(ctx context.Context, id string) ([]PendingChange, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage guard_principal_change_list v1 */
		SELECT id, principal_id, patch, status, requested_by, requested_by_user, reason,
			requested_at, expires_at
		FROM sage.guard_principal_changes WHERE principal_id = $1 AND status = 'pending'
		ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("agentguard: listing the changes of %s: %w", id, err)
	}
	defer rows.Close()
	out := []PendingChange{}
	for rows.Next() {
		var pc PendingChange
		if err := rows.Scan(&pc.ID, &pc.PrincipalID, &pc.Patch, &pc.Status, &pc.RequestedBy,
			&pc.RequestedByUser, &pc.Reason, &pc.RequestedAt, &pc.ExpiresAt); err != nil {
			return nil, fmt.Errorf("agentguard: reading a change of %s: %w", id, err)
		}
		out = append(out, pc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentguard: listing the changes of %s: %w", id, err)
	}
	return out, nil
}
