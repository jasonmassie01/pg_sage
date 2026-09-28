package sre

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// liveStates is the SQL list of states that hold a trigger.
const liveStates = `('queued', 'collecting', 'evaluating', 'needs_evidence', 'paused')`

// Create starts an investigation for a scoped trigger, or returns the
// live one for the same trigger (created false). A repeated idempotency
// key returns its investigation even after it concluded. A trigger whose
// waiting investigation passed its queue expiry gets a new one.
func (s *PostgresStore) Create(
	ctx context.Context, req StartRequest,
) (Investigation, bool, error) {
	if err := req.Validate(); err != nil {
		return Investigation{}, false, err
	}
	var inv Investigation
	created := false
	err := s.inTx(ctx, "create", func(tx pgx.Tx) error {
		if err := s.expireStaleTrigger(ctx, tx, req); err != nil {
			return err
		}
		if found, err := s.byIdempotencyKey(ctx, tx, req); err == nil {
			inv = found
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var err error
		inv, err = s.insertInvestigation(ctx, tx, req)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		inv, err = s.liveByFingerprint(ctx, tx, req)
		return err
	})
	return inv, created, err
}

func (s *PostgresStore) expireStaleTrigger(ctx context.Context, tx pgx.Tx,
	req StartRequest) error {
	_, err := tx.Exec(ctx, `UPDATE sage.sre_investigations
		SET state = 'expired', version = version + 1, failure_code = 'queue_expired',
		    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
		    updated_at = clock_timestamp()
		WHERE deployment_id = $1 AND database_id = $2 AND trigger_fingerprint = $3
		  AND state IN ('queued', 'needs_evidence', 'paused')
		  AND expires_at < clock_timestamp()`,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID), req.Fingerprint())
	return err
}

func (s *PostgresStore) byIdempotencyKey(ctx context.Context, tx pgx.Tx,
	req StartRequest) (Investigation, error) {
	if req.IdempotencyKey == "" {
		return Investigation{}, ErrNotFound
	}
	return scanInvestigation(tx.QueryRow(ctx, `SELECT `+invColumns+`
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND idempotency_key = $3`,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID),
		req.IdempotencyKey))
}

// insertInvestigation returns ErrNotFound when a live investigation of
// the same trigger (or the same idempotency key) already exists.
func (s *PostgresStore) insertInvestigation(ctx context.Context, tx pgx.Tx,
	req StartRequest) (Investigation, error) {
	var key *string
	if req.IdempotencyKey != "" {
		key = &req.IdempotencyKey
	}
	return scanInvestigation(tx.QueryRow(ctx, `INSERT INTO sage.sre_investigations
		(deployment_id, database_id, id, source_case_id, trigger_kind,
		 trigger_fingerprint, idempotency_key, state, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'queued',
		        clock_timestamp() + make_interval(secs => $8))
		ON CONFLICT DO NOTHING
		RETURNING `+invColumns,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID), string(NewUUID()),
		req.CaseID, string(req.TriggerKind), req.Fingerprint(), key,
		s.limits.QueueExpiry.Seconds()))
}

func (s *PostgresStore) liveByFingerprint(ctx context.Context, tx pgx.Tx,
	req StartRequest) (Investigation, error) {
	inv, err := scanInvestigation(tx.QueryRow(ctx, `SELECT `+invColumns+`
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND trigger_fingerprint = $3
		  AND state IN `+liveStates,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID), req.Fingerprint()))
	if errors.Is(err, ErrNotFound) {
		return s.byIdempotencyKey(ctx, tx, req)
	}
	return inv, err
}
