package sre

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// liveStates is the SQL list of states that hold a trigger.
const liveStates = `('queued', 'collecting', 'evaluating', 'needs_evidence', 'paused')`

// MaxAttempts bounds the runs of one idempotency key (sre-01): after the
// 5th expires, the trigger's polls return it and start nothing.
const MaxAttempts = 5

// ReasonQueueExpired is the failure code of a run that waited past its
// queue expiry without being claimed.
const ReasonQueueExpired = "queue_expired"

// Create starts an investigation for a scoped trigger, or returns the
// live one for the same trigger (created false). A repeated idempotency
// key returns its newest run, even after it concluded, with one
// exception: when that run expired in the queue or on stale evidence and
// the system asks again (the trigger is still open), attempt n+1 starts
// and supersedes it, up to MaxAttempts. A trigger whose waiting
// investigation passed its queue expiry gets a new one; a paused one
// never expires here.
func (s *PostgresStore) Create(
	ctx context.Context, req StartRequest,
) (Investigation, bool, error) {
	if err := req.Validate(); err != nil {
		return Investigation{}, false, err
	}
	var inv Investigation
	created := false
	err := s.inTx(ctx, "create", func(tx pgx.Tx) error {
		var err error
		inv, created, err = s.createTx(ctx, tx, req)
		return err
	})
	return inv, created, err
}

func (s *PostgresStore) createTx(ctx context.Context, tx pgx.Tx,
	req StartRequest) (Investigation, bool, error) {
	if err := s.expireStaleTrigger(ctx, tx, req); err != nil {
		return Investigation{}, false, err
	}
	prior, err := s.byIdempotencyKey(ctx, tx, req)
	switch {
	case errors.Is(err, ErrNotFound):
		prior = Investigation{}
	case err != nil:
		return Investigation{}, false, err
	case !retriesAfter(prior, req):
		return prior, false, nil
	case prior.Attempt >= MaxAttempts:
		return prior, false, s.recordExhausted(ctx, tx, prior)
	}
	inv, err := s.insertInvestigation(ctx, tx, req, prior)
	if err == nil {
		return inv, true, appendEvent(ctx, tx, inv.Scope, inv.ID, EventCreated,
			req.actor(), createdPayload(req, inv))
	}
	if !errors.Is(err, ErrNotFound) {
		return Investigation{}, false, err
	}
	inv, err = s.liveByFingerprint(ctx, tx, req)
	return inv, false, err
}

// retriesAfter reports whether a system request starts a new attempt
// after prior, the key's newest run: only an expiry that judged nothing
// (queue expiry, stale evidence) is retried, and never for an operator,
// whose key is HTTP idempotency.
func retriesAfter(prior Investigation, req StartRequest) bool {
	return req.automatic() && prior.State == StateExpired &&
		(prior.FailureCode == ReasonQueueExpired || prior.FailureCode == ReasonStaleEvidence)
}

func createdPayload(req StartRequest, inv Investigation) map[string]any {
	p := map[string]any{"trigger_kind": req.TriggerKind,
		"read_policy": req.ReadPolicy.Clone(),
		"case_id":     req.CaseID, "incident_id": req.IncidentID,
		"subject": req.Subject}
	if inv.Attempt > 1 {
		p["attempt"], p["supersedes"] = inv.Attempt, inv.Supersedes
	}
	return p
}

// recordExhausted records, once, that the key's last attempt expired and
// no further attempt starts. Once recorded, a poll only reads. Otherwise
// it takes the row lock and checks again in a new statement: in READ
// COMMITTED only a statement started after the lock was granted sees a
// racing poll's committed event.
func (s *PostgresStore) recordExhausted(ctx context.Context, tx pgx.Tx,
	last Investigation) error {
	args := []any{string(last.Scope.DeploymentID), string(last.Scope.DatabaseID),
		string(last.ID)}
	for _, lock := range []bool{false, true} {
		if lock {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM sage.sre_investigations
				WHERE deployment_id = $1 AND database_id = $2 AND id = $3
				FOR UPDATE`, args...); err != nil {
				return err
			}
		}
		var recorded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.sre_events
			WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
			  AND event_type = $4)`, append(args, EventAttemptsExhausted)...).
			Scan(&recorded); err != nil || recorded {
			return err
		}
	}
	return appendEvent(ctx, tx, last.Scope, last.ID, EventAttemptsExhausted, "system",
		map[string]any{"attempt": last.Attempt, "max_attempts": MaxAttempts,
			"reason": last.FailureCode})
}

// expireStaleTrigger ends the trigger's runs that waited (queued or
// needing evidence) past their queue expiry, each fenced and recorded as
// the system's transition. Paused runs are an operator's stop and never
// expire here.
func (s *PostgresStore) expireStaleTrigger(ctx context.Context, tx pgx.Tx,
	req StartRequest) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND trigger_fingerprint = $3
		  AND state IN ('queued', 'needs_evidence')
		  AND expires_at < clock_timestamp()
		ORDER BY id FOR UPDATE`,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID), req.Fingerprint())
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.setState(ctx, tx, req.Scope, UUID(id), StateExpired,
			ReasonQueueExpired, "system"); err != nil {
			return err
		}
	}
	return nil
}

// byIdempotencyKey returns the key's newest attempt.
func (s *PostgresStore) byIdempotencyKey(ctx context.Context, tx pgx.Tx,
	req StartRequest) (Investigation, error) {
	if req.IdempotencyKey == "" {
		return Investigation{}, ErrNotFound
	}
	return scanInvestigation(tx.QueryRow(ctx, `SELECT `+invColumns+`
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND idempotency_key = $3
		ORDER BY attempt DESC LIMIT 1`,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID),
		req.IdempotencyKey))
}

// insertInvestigation inserts the attempt after prior (the first when
// prior is zero). It returns ErrNotFound when a live investigation of the
// same trigger, or the same attempt of the key, already exists.
func (s *PostgresStore) insertInvestigation(ctx context.Context, tx pgx.Tx,
	req StartRequest, prior Investigation) (Investigation, error) {
	var key, incident, supersedes *string
	if req.IdempotencyKey != "" {
		key = &req.IdempotencyKey
	}
	if req.IncidentID != "" {
		incident = &req.IncidentID
	}
	if prior.ID != "" {
		id := string(prior.ID)
		supersedes = &id
	}
	return scanInvestigation(tx.QueryRow(ctx, `INSERT INTO sage.sre_investigations
		(deployment_id, database_id, id, source_case_id, trigger_kind,
		 trigger_fingerprint, idempotency_key, state, expires_at,
		 source_incident_id, subject, attempt, supersedes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'queued',
		        clock_timestamp() + make_interval(secs => $8), $9, $10, $11, $12)
		ON CONFLICT DO NOTHING
		RETURNING `+invColumns,
		string(req.Scope.DeploymentID), string(req.Scope.DatabaseID), string(NewUUID()),
		req.CaseID, string(req.TriggerKind), req.Fingerprint(), key,
		s.limits.QueueExpiry.Seconds(), incident, req.Subject, prior.Attempt+1,
		supersedes))
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
