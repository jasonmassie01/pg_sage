package sre

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// claimRow is the locked investigation row a claim decides on.
type claimRow struct {
	state      State
	held       bool
	orphanMS   int64
	activeMS   int64
	expiredNow bool
}

// Claim leases an investigation to worker. Exactly one of several racing
// workers wins (FOR UPDATE SKIP LOCKED plus an unexpired-lease check);
// each claim increments the fence token. An orphaned lease is charged
// in full before another worker resumes, and time is never given back.
func (s *PostgresStore) Claim(ctx context.Context, scope Scope, id, worker UUID) (Lease, error) {
	if err := validateIDs(scope, id, worker); err != nil {
		return Lease{}, err
	}
	var lease Lease
	err := s.inTx(ctx, "claim", func(tx pgx.Tx) error {
		row, err := s.lockForClaim(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := s.checkClaimable(ctx, tx, scope, id, row); err != nil {
			return err
		}
		lease, err = s.grantLease(ctx, tx, scope, id, worker, row)
		return err
	})
	return lease, err
}

func (s *PostgresStore) lockForClaim(ctx context.Context, tx pgx.Tx, scope Scope,
	id UUID) (claimRow, error) {
	var r claimRow
	var state string
	err := tx.QueryRow(ctx, `SELECT state,
		    COALESCE(lease_until > clock_timestamp(), false),
		    COALESCE(CASE WHEN lease_until <= clock_timestamp() THEN
		        (EXTRACT(EPOCH FROM lease_until - lease_started_at) * 1000)::int8 END, 0),
		    active_ms,
		    state IN ('queued', 'needs_evidence') AND expires_at < clock_timestamp()
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3
		FOR UPDATE SKIP LOCKED`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).
		Scan(&state, &r.held, &r.orphanMS, &r.activeMS, &r.expiredNow)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.Get(ctx, scope, id); gerr != nil {
			return r, gerr
		}
		return r, ErrLeaseUnavailable // row locked by a racing claimer
	}
	r.state = State(state)
	return r, err
}

func (s *PostgresStore) checkClaimable(ctx context.Context, tx pgx.Tx, scope Scope,
	id UUID, r claimRow) error {
	switch {
	case r.state.Terminal():
		return ErrTerminal
	case r.held:
		return ErrLeaseUnavailable
	case r.state == StatePaused:
		return ErrInvalidTransition
	case r.expiredNow:
		if _, err := s.setState(ctx, tx, scope, id, StateExpired, "queue_expired",
			"system"); err != nil {
			return err
		}
		return commitThen{ErrTerminal}
	}
	limit := s.limits.MaxActive.Milliseconds()
	if r.activeMS+r.orphanMS >= limit {
		// A dead worker used the whole active-time budget: charge it and
		// end the investigation, so it does not stay "collecting".
		if _, err := s.setState(ctx, tx, scope, id, StateFailed,
			"budget_exhausted", "system"); err != nil {
			return err
		}
		return commitThen{ErrBudgetExhausted}
	}
	return nil
}

func (s *PostgresStore) grantLease(ctx context.Context, tx pgx.Tx, scope Scope, id,
	worker UUID, r claimRow) (Lease, error) {
	active := r.activeMS + r.orphanMS
	remaining := time.Duration(s.limits.MaxActive.Milliseconds()-active) * time.Millisecond
	ttl := s.limits.LeaseTTL
	if ttl > remaining {
		ttl = remaining
	}
	l := Lease{Scope: scope, InvestigationID: id, WorkerID: worker}
	err := tx.QueryRow(ctx, `UPDATE sage.sre_investigations
		SET fence_token = fence_token + 1, version = version + 1,
		    lease_owner = $4, lease_started_at = clock_timestamp(),
		    lease_until = clock_timestamp() + make_interval(secs => $5),
		    segment_deadline = clock_timestamp() + make_interval(secs => $6),
		    active_ms = $7, updated_at = clock_timestamp(),
		    state = CASE WHEN state IN ('queued', 'needs_evidence')
		                 THEN 'collecting' ELSE state END
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3
		RETURNING version, fence_token, lease_until, segment_deadline`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), string(worker),
		ttl.Seconds(), remaining.Seconds(), active).
		Scan(&l.Version, &l.Fence, &l.Until, &l.SegmentDeadline)
	if err != nil {
		return l, err
	}
	return l, appendEvent(ctx, tx, scope, id, EventClaimed, workerActor(l),
		map[string]any{"fence": l.Fence, "orphan_ms": r.orphanMS})
}

// leaseGuard is the SQL predicate every lease-bound write repeats.
const leaseGuard = `deployment_id = $1 AND database_id = $2 AND id = $3
	AND lease_owner = $4 AND fence_token = $5 AND lease_until > clock_timestamp()`

func leaseArgs(l Lease) []any {
	return []any{string(l.Scope.DeploymentID), string(l.Scope.DatabaseID),
		string(l.InvestigationID), string(l.WorkerID), l.Fence}
}

// Heartbeat extends a live lease, never past its segment deadline.
func (s *PostgresStore) Heartbeat(ctx context.Context, lease Lease) (Lease, error) {
	if err := lease.validate(); err != nil {
		return Lease{}, err
	}
	out := lease
	err := s.pool.QueryRow(ctx, `UPDATE sage.sre_investigations
		SET lease_until = LEAST(clock_timestamp() + make_interval(secs => $6),
		                        segment_deadline)
		WHERE `+leaseGuard+`
		RETURNING lease_until, segment_deadline`,
		append(leaseArgs(lease), s.limits.LeaseTTL.Seconds())...).
		Scan(&out.Until, &out.SegmentDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseLost
	}
	return out, storeErr(ctx, "heartbeat", err)
}

// chargeSQL charges the time since the lease started (at most until
// the lease ended), capped at $cap milliseconds.
const chargeSQL = `LEAST($%d::int8, active_ms + COALESCE((EXTRACT(EPOCH FROM
	LEAST(clock_timestamp(), lease_until) - lease_started_at) * 1000)::int8, 0))`

// expiresSQL refreshes the queue expiry when an investigation waits again.
const expiresSQL = `CASE WHEN $%d IN ('queued', 'needs_evidence', 'paused')
	THEN clock_timestamp() + make_interval(secs => $%d) ELSE expires_at END`
