package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// lockedChange is a pending-change row locked for a confirm.
type lockedChange struct {
	patch           Patch
	status          string
	requestedBy     string
	requestedByUser int
	expired         bool
}

// ConfirmChange applies the pending change req.ChangeID as the second
// admin and writes agent_change_confirmed in the same transaction. A
// change past its TTL is marked expired (that much commits) and refused
// with ErrChangeExpired.
func (s *Store) ConfirmChange(ctx context.Context, req ChangeConfirm) (Principal, error) {
	if req.ChangeID <= 0 {
		return Principal{}, fmt.Errorf("%w: change %d", ErrChangeNotFound, req.ChangeID)
	}
	err := checkChangeShape(req.PrincipalID, req.Patch, req.Actor, req.Audit,
		auth.AuditAgentChangeConfirmed)
	if err != nil {
		return Principal{}, err
	}
	if err := s.ready(); err != nil {
		return Principal{}, err
	}
	expired := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var txErr error
		expired, txErr = confirmInTx(ctx, tx, req)
		return txErr
	})
	if err != nil {
		return Principal{}, err
	}
	if expired {
		return Principal{}, fmt.Errorf("%w: change %d", ErrChangeExpired, req.ChangeID)
	}
	return s.Get(ctx, req.PrincipalID)
}

// confirmInTx returns expired=true (and no error, so the expiry commits)
// for a change past its TTL.
func confirmInTx(ctx context.Context, tx pgx.Tx, req ChangeConfirm) (bool, error) {
	cur, err := lockForChange(ctx, tx, req.PrincipalID)
	if err != nil {
		return false, err
	}
	ch, err := lockChange(ctx, tx, req.PrincipalID, req.ChangeID)
	if err != nil {
		return false, err
	}
	if ch.status != "pending" {
		return false, fmt.Errorf("%w: change %d is %s", ErrChangeNotPending, req.ChangeID,
			ch.status)
	}
	if ch.expired {
		return true, closeChange(ctx, tx, req, "expired")
	}
	if err := checkConfirmer(req, cur.shape, ch); err != nil {
		return false, err
	}
	a := req.Audit
	a.Event = auth.AuditAgentChangeConfirmed
	err = applyLocked(ctx, tx, req.PrincipalID, cur, ch.patch, a, map[string]any{
		"change_id": req.ChangeID, "requested_by_user": ch.requestedByUser})
	if err != nil {
		return false, err
	}
	return false, closeChange(ctx, tx, req, "applied")
}

// checkConfirmer refuses the requester, the sponsor (current or the one
// the patch names) and a patch other than the requested one.
func checkConfirmer(req ChangeConfirm, cur shape, ch lockedChange) error {
	by := req.Audit.ActorUserID
	switch {
	case ch.requestedByUser == by:
		return fmt.Errorf("%w (change %d, requested by %s)", ErrSecondAdminRequired,
			req.ChangeID, ch.requestedBy)
	case cur.SponsorUserID != nil && *cur.SponsorUserID == by,
		ch.patch.SponsorUserID != nil && *ch.patch.SponsorUserID == by:
		return fmt.Errorf("%w (change %d)", ErrSponsorCannotApprove, req.ChangeID)
	case !ch.patch.equal(req.Patch):
		return fmt.Errorf("%w (change %d)", ErrChangeMismatch, req.ChangeID)
	}
	return nil
}

func lockChange(ctx context.Context, tx pgx.Tx, principal string,
	id int64) (lockedChange, error) {
	var ch lockedChange
	var doc PatchDoc
	err := tx.QueryRow(ctx, `/* pg_sage guard_principal_change_lock v1 */
		SELECT patch, status, requested_by, requested_by_user, expires_at <= now()
		FROM sage.guard_principal_changes WHERE id = $1 AND principal_id = $2
		FOR UPDATE`, id, principal).
		Scan(&doc, &ch.status, &ch.requestedBy, &ch.requestedByUser, &ch.expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ch, fmt.Errorf("%w: change %d of principal %s", ErrChangeNotFound, id,
			principal)
	}
	if err != nil {
		return ch, fmt.Errorf("agentguard: locking change %d: %w", id, err)
	}
	ch.patch = doc.patch()
	return ch, nil
}

func closeChange(ctx context.Context, tx pgx.Tx, req ChangeConfirm, status string) error {
	_, err := tx.Exec(ctx, `/* pg_sage guard_principal_change_close v1 */
		UPDATE sage.guard_principal_changes SET status = $2, decided_by = $3,
			decided_by_user = $4, decided_at = now()
		WHERE id = $1`, req.ChangeID, status, req.Actor, req.Audit.ActorUserID)
	if err != nil {
		return fmt.Errorf("agentguard: closing change %d as %s: %w", req.ChangeID, status, err)
	}
	return nil
}
