package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Release ends a worker's lease, charging its active time, and moves the
// investigation to next (needs_evidence, concluded, inconclusive, paused
// or failed as the state machine allows).
func (s *PostgresStore) Release(ctx context.Context, lease Lease,
	next State) (Investigation, error) {
	if err := lease.validate(); err != nil {
		return Investigation{}, err
	}
	var inv Investigation
	err := s.inTx(ctx, "release", func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM sage.sre_investigations
			WHERE `+leaseGuard+` FOR UPDATE`, leaseArgs(lease)...).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if !CanTransition(State(state), next) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, next)
		}
		inv, err = scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
			SET state = $6, active_ms = `+fmt.Sprintf(chargeSQL, 7)+`,
			    expires_at = `+fmt.Sprintf(expiresSQL, 6, 8)+`,
			    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
			    version = version + 1, updated_at = clock_timestamp()
			WHERE `+leaseGuard+` RETURNING `+invColumns,
			append(leaseArgs(lease), string(next), s.limits.MaxActive.Milliseconds(),
				s.limits.QueueExpiry.Seconds())...))
		return err
	})
	return inv, err
}

// Pause stops work on a live investigation, charging any running lease
// and fencing out its worker. History and consumed budget are kept.
func (s *PostgresStore) Pause(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.operatorTransition(ctx, scope, id, version, StatePaused)
}

// Resume re-queues a paused investigation (a new revision). Budgets are
// not refilled.
func (s *PostgresStore) Resume(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.operatorTransition(ctx, scope, id, version, StateQueued)
}

// Stop cancels a live investigation; its evidence and steps are kept.
func (s *PostgresStore) Stop(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.operatorTransition(ctx, scope, id, version, StateCancelled)
}

// operatorTransition applies an operator's change under an If-Match
// style version precondition.
func (s *PostgresStore) operatorTransition(ctx context.Context, scope Scope, id UUID,
	version int64, to State) (Investigation, error) {
	if err := validateIDs(scope, id); err != nil {
		return Investigation{}, err
	}
	var inv Investigation
	err := s.inTx(ctx, "transition", func(tx pgx.Tx) error {
		var state string
		var current int64
		err := tx.QueryRow(ctx, `SELECT state, version FROM sage.sre_investigations
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id)).
			Scan(&state, &current)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case current != version:
			return fmt.Errorf("%w: version %d, expected %d", ErrVersionConflict,
				current, version)
		case !CanTransition(State(state), to):
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, to)
		}
		inv, err = s.setState(ctx, tx, scope, id, to, "")
		return err
	})
	return inv, err
}

// setState moves an investigation to state, charging and clearing any
// lease and advancing the fence so a stale worker cannot commit.
func (s *PostgresStore) setState(ctx context.Context, tx pgx.Tx, scope Scope, id UUID,
	to State, failure string) (Investigation, error) {
	return scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
		SET state = $4, active_ms = `+fmt.Sprintf(chargeSQL, 5)+`,
		    expires_at = `+fmt.Sprintf(expiresSQL, 4, 6)+`,
		    failure_code = COALESCE(NULLIF($7, ''), failure_code),
		    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
		    fence_token = fence_token + 1, version = version + 1,
		    updated_at = clock_timestamp()
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3
		RETURNING `+invColumns,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), string(to),
		s.limits.MaxActive.Milliseconds(), s.limits.QueueExpiry.Seconds(), failure))
}
