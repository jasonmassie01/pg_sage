package sre

import (
	"context"
	"encoding/json"
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
			    concluded_at = CASE WHEN $6 IN `+terminalStates+`
			                        THEN clock_timestamp() ELSE concluded_at END,
			    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
			    version = version + 1, updated_at = clock_timestamp()
			WHERE `+leaseGuard+` RETURNING `+invColumns,
			append(leaseArgs(lease), string(next), s.limits.MaxActive.Milliseconds(),
				s.limits.QueueExpiry.Seconds())...))
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, lease.Scope, lease.InvestigationID, EventTransition,
			workerActor(lease), map[string]any{"from": state, "to": next})
	})
	return inv, err
}

// Pause stops work on a live investigation, charging any running lease
// and fencing out its worker. History and consumed budget are kept.
func (s *PostgresStore) Pause(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.OperatorTransition(ctx, scope, id, version, StatePaused, "system")
}

// Resume re-queues a paused investigation (a new revision). Budgets are
// not refilled.
func (s *PostgresStore) Resume(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.OperatorTransition(ctx, scope, id, version, StateQueued, "system")
}

// Stop cancels a live investigation; its evidence and steps are kept.
func (s *PostgresStore) Stop(ctx context.Context, scope Scope, id UUID,
	version int64) (Investigation, error) {
	return s.OperatorTransition(ctx, scope, id, version, StateCancelled, "system")
}

// OperatorTransition applies an actor's change under an If-Match style
// version precondition and records it, attributed, in the event chain.
func (s *PostgresStore) OperatorTransition(ctx context.Context, scope Scope, id UUID,
	version int64, to State, actor string) (Investigation, error) {
	if err := validateIDs(scope, id); err != nil {
		return Investigation{}, err
	}
	if err := checkText("actor", actor, true, 128); err != nil {
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
		inv, err = s.setState(ctx, tx, scope, id, to, "", actor)
		return err
	})
	return inv, err
}

// ExpireStale ends a leased run whose stored evidence is too old to
// conclude on (sre-02): in one transaction, fenced on the lease, it
// writes the summary (reason and evidence age) and moves the run to
// expired with failure code stale_evidence, recorded as the system's
// transition on the event chain. No hypotheses are written: expired is
// not a conclusion. A lost lease is ErrLeaseLost and changes nothing.
func (s *PostgresStore) ExpireStale(ctx context.Context, lease Lease,
	sum Summary) (Investigation, error) {
	if err := lease.validate(); err != nil {
		return Investigation{}, err
	}
	raw, err := json.Marshal(sum)
	if err != nil {
		return Investigation{}, fmt.Errorf("%w: stale summary: %v", ErrInvalidRequest, err)
	}
	var inv Investigation
	err = s.inTx(ctx, "expire_stale", func(tx pgx.Tx) error {
		state, _, err := s.lockLeased(ctx, tx, lease)
		if err != nil {
			return err
		}
		if !CanTransition(state, StateExpired) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, StateExpired)
		}
		tag, err := tx.Exec(ctx, `UPDATE sage.sre_investigations
			SET summary = $6::jsonb WHERE `+leaseGuard,
			append(leaseArgs(lease), string(raw))...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 { // the lease ran out after the row was locked
			return ErrLeaseLost
		}
		inv, err = s.setState(ctx, tx, lease.Scope, lease.InvestigationID, StateExpired,
			ReasonStaleEvidence, "system")
		return err
	})
	return inv, err
}

// setState moves an investigation to state, charging and clearing any
// lease and advancing the fence so a stale worker cannot commit. It
// records the transition, by actor, in the event chain.
func (s *PostgresStore) setState(ctx context.Context, tx pgx.Tx, scope Scope, id UUID,
	to State, failure, actor string) (Investigation, error) {
	inv, err := scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
		SET state = $4, active_ms = `+fmt.Sprintf(chargeSQL, 5)+`,
		    expires_at = `+fmt.Sprintf(expiresSQL, 4, 6)+`,
		    failure_code = COALESCE(NULLIF($7, ''), failure_code),
		    concluded_at = CASE WHEN $4 IN `+terminalStates+`
		                        THEN clock_timestamp() ELSE concluded_at END,
		    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
		    fence_token = fence_token + 1, version = version + 1,
		    updated_at = clock_timestamp()
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3
		RETURNING `+invColumns,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), string(to),
		s.limits.MaxActive.Milliseconds(), s.limits.QueueExpiry.Seconds(), failure))
	if err != nil {
		return inv, err
	}
	payload := map[string]any{"to": to}
	if failure != "" {
		payload["reason"] = failure
	}
	return inv, appendEvent(ctx, tx, scope, id, EventTransition, actor, payload)
}

// terminalStates is the SQL list of final states.
const terminalStates = `('concluded', 'inconclusive', 'cancelled', 'expired', 'failed')`
