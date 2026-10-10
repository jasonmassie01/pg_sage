package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Unfreeze and release (§6.10 "after a kill", §6.11). Lifting a freeze
// widens, so it needs people: one admin for an operator freeze; two after
// a kill, the second neither the first nor the principal's sponsor,
// unless agents.single_operator_mode lets one admin do it with a reason.
// The first admin's call records a pending request; the second admin's
// call (within the approval TTL) applies it. Every unfreeze restores the
// roles' prior attributes and rotates each broker credential, through
// Executor.Apply as guard_unfreeze (not narrowing: the emergency stop and
// the trust level bind it).

// UnfreezeRequest is POST /api/v1/agents/{id}/unfreeze by a signed-in
// admin (ActorUserID is the sage.users id).
type UnfreezeRequest struct {
	PrincipalID string
	Reason      string
	Actor       string
	ActorUserID int
}

// Validate checks the request's shape.
func (r UnfreezeRequest) Validate() error {
	if !ValidID(r.PrincipalID) {
		return invalid("principal id %q is not valid", r.PrincipalID)
	}
	return checkApprover(r.Reason, r.Actor, r.ActorUserID)
}

func checkApprover(reason, actor string, userID int) error {
	if err := checkText("reason", reason, 0, maxReasonLen); err != nil {
		return err
	}
	if err := checkText("actor", actor, 1, maxActorLen); err != nil {
		return err
	}
	if userID <= 0 {
		return fmt.Errorf("%w: a signed-in admin must unfreeze", ErrApprovalRequired)
	}
	return nil
}

// ReleaseRequest lifts a fleet (scope all) or database flag a kill set.
type ReleaseRequest struct {
	Scope       KillScope
	Database    string
	Reason      string
	Actor       string
	ActorUserID int
}

// Validate checks the request's shape.
func (r ReleaseRequest) Validate() error {
	switch r.Scope {
	case KillScopeAll:
		if r.Database != "" {
			return invalid("scope all takes no database")
		}
	case KillScopeDatabase:
		if err := checkText("database", r.Database, 1, 200); err != nil {
			return err
		}
	default:
		return invalid("scope %q must be all or database", r.Scope)
	}
	return checkApprover(r.Reason, r.Actor, r.ActorUserID)
}

// UnfreezeResult is what an unfreeze or release call did.
type UnfreezeResult struct {
	Applied        bool              `json:"applied"`
	Pending        bool              `json:"pending"`
	RequestID      int64             `json:"request_id,omitempty"`
	RequestedBy    string            `json:"requested_by,omitempty"`
	Quorum         int               `json:"quorum"`
	SingleOperator bool              `json:"single_operator,omitempty"`
	Clusters       []UnfreezeCluster `json:"clusters,omitempty"`
}

// UnfreezeCluster is the guard_unfreeze action on one cluster.
type UnfreezeCluster struct {
	ClusterKey string     `json:"cluster_key"`
	ActionID   int64      `json:"action_id"`
	Rotated    bool       `json:"rotated"`
	Restored   PriorAttrs `json:"restored"`
}

// openFreeze is the open guard_freezes row being lifted.
type openFreeze struct {
	id     int64
	killed bool
	scope  string
	target string
}

// signoff is the people check of one lift.
type signoff struct {
	freeze  openFreeze
	quorum  int
	single  bool
	sponsor *int
	reason  string
	actor   string
	userID  int
}

// Unfreeze lifts a principal's freeze.
func (s *Switch) Unfreeze(ctx context.Context, req UnfreezeRequest) (UnfreezeResult, error) {
	if err := req.Validate(); err != nil {
		return UnfreezeResult{}, err
	}
	pool := s.store.Pool()
	if pool == nil {
		return UnfreezeResult{}, ErrUnavailable
	}
	p, err := s.store.Get(ctx, req.PrincipalID)
	if err != nil {
		return UnfreezeResult{}, err
	}
	if !p.Frozen() {
		return UnfreezeResult{}, fmt.Errorf("%w: principal %s is %s", ErrNotFrozen, p.ID,
			p.Status)
	}
	plan, err := s.unfreezePlan(ctx, p.ID)
	if err != nil {
		return UnfreezeResult{}, err
	}
	var res UnfreezeResult
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var txErr error
		res, txErr = s.unfreezeInTx(ctx, tx, p, req, plan)
		return txErr
	})
	return res, err
}

func (s *Switch) unfreezeInTx(ctx context.Context, tx pgx.Tx, p Principal,
	req UnfreezeRequest, plan []unfreezeRun) (UnfreezeResult, error) {
	if err := lockFrozenPrincipal(ctx, tx, p.ID); err != nil {
		return UnfreezeResult{}, err
	}
	fr, err := lockOpenFreeze(ctx, tx, "principal", p.ID, false)
	if err != nil {
		return UnfreezeResult{}, err
	}
	so := s.signoffFor(fr, req.Reason, req.Actor, req.ActorUserID)
	so.sponsor = p.SponsorUserID
	res, proceed, err := s.signOff(ctx, tx, so)
	if err != nil || !proceed {
		return res, err
	}
	for _, run := range plan {
		c, err := run.apply(ctx, s, req, res.RequestID)
		if err != nil {
			return UnfreezeResult{}, err
		}
		res.Clusters = append(res.Clusters, c)
	}
	if err := liftFreeze(ctx, tx, so, res.RequestID); err != nil {
		return UnfreezeResult{}, err
	}
	if _, err := tx.Exec(ctx, `/* pg_sage guard_unfreeze v1 */
		UPDATE sage.guard_principals SET status = 'active', frozen_reason = '',
			updated_at = now() WHERE id = $1 AND status = 'frozen'`, p.ID); err != nil {
		return UnfreezeResult{}, fmt.Errorf("agentguard: activating %s: %w", p.ID, err)
	}
	res.Applied, res.Pending = true, false
	return res, nil
}

// Release lifts a fleet or database flag.
func (s *Switch) Release(ctx context.Context, req ReleaseRequest) (UnfreezeResult, error) {
	if err := req.Validate(); err != nil {
		return UnfreezeResult{}, err
	}
	pool := s.store.Pool()
	if pool == nil {
		return UnfreezeResult{}, ErrUnavailable
	}
	scope, target := "fleet", ""
	if req.Scope == KillScopeDatabase {
		scope, target = "database", req.Database
	}
	var res UnfreezeResult
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		fr, err := lockOpenFreeze(ctx, tx, scope, target, true)
		if err != nil {
			return err
		}
		fr.killed = true // a flag is only set by a kill: always two people
		so := s.signoffFor(fr, req.Reason, req.Actor, req.ActorUserID)
		var proceed bool
		if res, proceed, err = s.signOff(ctx, tx, so); err != nil || !proceed {
			return err
		}
		res.Applied, res.Pending = true, false
		return liftFreeze(ctx, tx, so, res.RequestID)
	})
	return res, err
}

func (s *Switch) signoffFor(fr openFreeze, reason, actor string, userID int) signoff {
	so := signoff{freeze: fr, quorum: 1, reason: reason, actor: actor, userID: userID}
	if fr.killed {
		so.quorum = 2
		if s.cfg.SingleOperatorMode {
			so.quorum, so.single = 1, true
		}
	}
	return so
}

func lockFrozenPrincipal(ctx context.Context, tx pgx.Tx, id string) error {
	var status string
	err := tx.QueryRow(ctx, `/* pg_sage guard_unfreeze v1 */
		SELECT status FROM sage.guard_principals WHERE id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: principal %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("agentguard: locking principal %s: %w", id, err)
	}
	if status != string(StatusFrozen) {
		return fmt.Errorf("%w: principal %s is %s", ErrNotFrozen, id, status)
	}
	return nil
}

// lockOpenFreeze locks the open freeze row of (scope, target). A frozen
// principal without one (frozen by other means) is an operator freeze;
// a flag without one is ErrNotFrozen.
func lockOpenFreeze(ctx context.Context, tx pgx.Tx, scope, target string,
	required bool) (openFreeze, error) {
	fr := openFreeze{scope: scope, target: target}
	var killID *int64
	err := tx.QueryRow(ctx, `/* pg_sage guard_unfreeze v1 */
		SELECT id, kill_id FROM sage.guard_freezes
		WHERE scope = $1 AND target = $2 AND cleared_at IS NULL FOR UPDATE`, scope, target).
		Scan(&fr.id, &killID)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && required:
		return fr, fmt.Errorf("%w: no open %s freeze %q", ErrNotFrozen, scope, target)
	case errors.Is(err, pgx.ErrNoRows):
		return fr, nil
	case err != nil:
		return fr, fmt.Errorf("agentguard: locking the %s freeze: %w", scope, err)
	}
	fr.killed = killID != nil
	return fr, nil
}

// signOff records or completes the people check. proceed is true when the
// caller completes the quorum; otherwise res is the pending request.
func (s *Switch) signOff(ctx context.Context, tx pgx.Tx, so signoff) (UnfreezeResult, bool,
	error) {
	res := UnfreezeResult{Quorum: so.quorum, SingleOperator: so.single}
	if so.single && so.reason == "" {
		return res, false, invalid("single-operator mode needs a reason for the review queue")
	}
	if so.quorum == 1 || so.freeze.id == 0 {
		return res, true, nil
	}
	pend, err := pendingRequest(ctx, tx, so.freeze.id)
	if err != nil {
		return res, false, err
	}
	if pend == nil || pend.userID == so.userID {
		if pend == nil {
			if pend, err = s.newRequest(ctx, tx, so); err != nil {
				return res, false, err
			}
		}
		res.Pending, res.RequestID, res.RequestedBy = true, pend.id, pend.by
		return res, false, nil
	}
	if so.sponsor != nil && *so.sponsor == so.userID {
		return res, false, ErrSponsorCannotApprove
	}
	res.RequestID, res.RequestedBy = pend.id, pend.by
	return res, true, nil
}

type pendingReq struct {
	id     int64
	by     string
	userID int
}

// pendingRequest is the freeze's live pending request; an expired one is
// marked expired and is no request.
func pendingRequest(ctx context.Context, tx pgx.Tx, freezeID int64) (*pendingReq, error) {
	var p pendingReq
	var expired bool
	err := tx.QueryRow(ctx, `/* pg_sage guard_unfreeze v1 */
		SELECT id, requested_by, requested_by_user, expires_at <= now()
		FROM sage.guard_unfreeze_requests WHERE freeze_id = $1 AND status = 'pending'
		FOR UPDATE`, freezeID).Scan(&p.id, &p.by, &p.userID, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading the pending unfreeze: %w", err)
	}
	if !expired {
		return &p, nil
	}
	if _, err := tx.Exec(ctx, `/* pg_sage guard_unfreeze v1 */
		UPDATE sage.guard_unfreeze_requests SET status = 'expired', decided_at = now()
		WHERE id = $1`, p.id); err != nil {
		return nil, fmt.Errorf("agentguard: expiring an unfreeze request: %w", err)
	}
	return nil, nil
}

func (s *Switch) newRequest(ctx context.Context, tx pgx.Tx, so signoff) (*pendingReq,
	error) {
	p := pendingReq{by: so.actor, userID: so.userID}
	err := tx.QueryRow(ctx, `/* pg_sage guard_unfreeze v1 */
		INSERT INTO sage.guard_unfreeze_requests (scope, target, freeze_id, requested_by,
			requested_by_user, reason, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		RETURNING id`, so.freeze.scope, so.freeze.target, so.freeze.id, so.actor, so.userID,
		so.reason, s.cfg.ApprovalTTL.Seconds()).Scan(&p.id)
	if err != nil {
		return nil, fmt.Errorf("agentguard: recording the unfreeze request: %w", err)
	}
	return &p, nil
}

// liftFreeze clears the freeze and closes its request.
func liftFreeze(ctx context.Context, tx pgx.Tx, so signoff, requestID int64) error {
	if so.freeze.id == 0 {
		return nil
	}
	by := so.actor
	if so.single {
		by += " (single operator: " + so.reason + ")"
	}
	_, err := tx.Exec(ctx, `/* pg_sage guard_unfreeze v1 */
		UPDATE sage.guard_freezes SET cleared_at = now(), cleared_by = left($2, 2000)
		WHERE id = $1`, so.freeze.id, by)
	if err == nil && requestID > 0 {
		_, err = tx.Exec(ctx, `/* pg_sage guard_unfreeze v1 */
			UPDATE sage.guard_unfreeze_requests SET status = 'applied', approved_by = $2,
				approved_by_user = $3, decided_at = now() WHERE id = $1`, requestID, so.actor,
			so.userID)
	}
	if err != nil {
		return fmt.Errorf("agentguard: lifting the freeze: %w", err)
	}
	return nil
}

