package sre

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CommitStep records one step and its evidence under the lease: the
// lease, fence and deadline are checked in the same transaction as the
// append-only inserts. A repeated idempotency key is a no-op; a stale or
// expired lease commits nothing (ErrLeaseLost).
func (s *PostgresStore) CommitStep(ctx context.Context, lease Lease,
	step StepResult) (Investigation, error) {
	if err := lease.validate(); err != nil {
		return Investigation{}, err
	}
	if err := checkText("step key", step.IdempotencyKey, true, 160); err != nil {
		return Investigation{}, err
	}
	var inv Investigation
	err := s.inTx(ctx, "commit step", func(tx pgx.Tx) error {
		state, probeCount, err := s.lockLeased(ctx, tx, lease)
		if err != nil {
			return err
		}
		if dup, err := stepExists(ctx, tx, lease, step.IdempotencyKey); err != nil || dup {
			if err == nil {
				inv, err = scanInvestigation(tx.QueryRow(ctx, `SELECT `+invColumns+`
					FROM sage.sre_investigations WHERE `+leaseGuard, leaseArgs(lease)...))
			}
			return err
		}
		next := step.NextState
		if !CanTransition(state, next) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, next)
		}
		if probeCount+len(step.Results) > s.limits.MaxProbes {
			return fmt.Errorf("%w: %d probes over the cap of %d", ErrBudgetExhausted,
				probeCount+len(step.Results), s.limits.MaxProbes)
		}
		if err := insertStep(ctx, tx, lease, step); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, lease.Scope, lease.InvestigationID, EventStep,
			workerActor(lease), map[string]any{"key": step.IdempotencyKey,
				"probes": len(step.Results), "next": step.NextState}); err != nil {
			return err
		}
		inv, err = scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
			SET state = $6, probe_count = probe_count + $7, version = version + 1,
			    updated_at = clock_timestamp()
			WHERE `+leaseGuard+` RETURNING `+invColumns,
			append(leaseArgs(lease), string(next), len(step.Results))...))
		if errors.Is(err, ErrNotFound) {
			return ErrLeaseLost // the lease expired inside the transaction
		}
		return err
	})
	return inv, err
}

// lockLeased locks the investigation if the lease is still valid.
func (s *PostgresStore) lockLeased(ctx context.Context, tx pgx.Tx,
	lease Lease) (State, int, error) {
	var state string
	var count int
	err := tx.QueryRow(ctx, `SELECT state, probe_count FROM sage.sre_investigations
		WHERE `+leaseGuard+` AND segment_deadline > clock_timestamp() FOR UPDATE`,
		leaseArgs(lease)...).Scan(&state, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrLeaseLost
	}
	return State(state), count, err
}

func stepExists(ctx context.Context, tx pgx.Tx, lease Lease, key string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM sage.sre_steps
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		  AND idempotency_key = $4`, string(lease.Scope.DeploymentID),
		string(lease.Scope.DatabaseID), string(lease.InvestigationID), key).Scan(&n)
	return n > 0, err
}

func insertStep(ctx context.Context, tx pgx.Tx, lease Lease, step StepResult) error {
	stepID := NewUUID()
	sc := lease.Scope
	_, err := tx.Exec(ctx, `INSERT INTO sage.sre_steps
		(deployment_id, database_id, investigation_id, id, sequence, idempotency_key,
		 fence_token, probe_count, next_state, error_code)
		SELECT $1, $2, $3, $4, COALESCE(max(sequence), 0) + 1, $5, $6, $7, $8,
		       NULLIF($9, '')
		FROM sage.sre_steps
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3`,
		string(sc.DeploymentID), string(sc.DatabaseID), string(lease.InvestigationID),
		string(stepID), step.IdempotencyKey, lease.Fence, len(step.Results),
		string(step.NextState), step.ErrorCode)
	if err != nil {
		return err
	}
	for _, res := range step.Results {
		if _, _, err := insertEvidence(ctx, tx, lease.Scope, lease.InvestigationID, stepID,
			sourceProbe, res); err != nil {
			return err
		}
	}
	return nil
}

// capabilityState maps a probe status to the evidence capability state.
func capabilityState(st probes.Status) string {
	switch st {
	case probes.StatusOK, probes.StatusEmpty:
		return "available"
	case probes.StatusNoPrivilege:
		return "permission_denied"
	case probes.StatusUnsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// Evidence source kinds: a lease-bound probe step, or an operator's
// re-read of a finished investigation (AppendRefreshEvidence).
const (
	sourceProbe           = "probe"
	SourceOperatorRefresh = "operator_refresh"
)

// insertEvidence writes one evidence row of a step and returns its id and
// payload hash.
func insertEvidence(ctx context.Context, tx pgx.Tx, sc Scope, invID, stepID UUID,
	source string, res probes.Result) (UUID, []byte, error) {
	payload, err := canonicalPayload(res)
	if err != nil {
		return "", nil, fmt.Errorf("%w: evidence payload: %v", ErrInvalidRequest, err)
	}
	sum := sha256.Sum256(payload)
	reason := res.Reason
	capability := capabilityState(res.Status)
	if capability != "available" && reason == "" {
		reason = string(res.Status)
	}
	var observed *time.Time
	if !res.ObservedAt.IsZero() {
		observed = &res.ObservedAt
	}
	version := res.Version
	if version == "" {
		version = "unknown"
	}
	id := NewUUID()
	_, err = tx.Exec(ctx, `INSERT INTO sage.sre_evidence
		(deployment_id, database_id, investigation_id, id, step_id, source_kind,
		 probe_id, probe_version, observed_at, capability_state, reason_code,
		 payload_version, classification, payload, sha256)
		VALUES ($1, $2, $3, $4, $5, $13, $6, $7, $8, $9, NULLIF($10, ''), 1,
		        'redacted', $11, $12)`,
		string(sc.DeploymentID), string(sc.DatabaseID), string(invID),
		string(id), string(stepID), string(res.ProbeID), version, observed,
		capability, reason, payload, sum[:], source)
	return id, sum[:], err
}

// Evidence lists an investigation's evidence in step order.
func (s *PostgresStore) Evidence(ctx context.Context, scope Scope, id UUID) ([]Evidence, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	return s.evidence(ctx, scope, id, nil, nil)
}

// StepEvidence lists the evidence one step stored, by its idempotency
// key, in the order Evidence lists it: the investigator reads back the
// read it just committed without re-reading every payload. A key with no
// stored evidence is ErrNotFound.
func (s *PostgresStore) StepEvidence(ctx context.Context, scope Scope, id UUID,
	key string) ([]Evidence, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	if err := checkText("step key", key, true, 160); err != nil {
		return nil, err
	}
	out, err := s.evidence(ctx, scope, id, nil, &key)
	if err == nil && len(out) == 0 {
		err = fmt.Errorf("%w: no evidence of step %s", ErrNotFound, key)
	}
	return out, err
}

// StepKeys lists the idempotency keys of the steps that stored evidence,
// without reading any payload: a resumed run skips those steps.
func (s *PostgresStore) StepKeys(ctx context.Context, scope Scope,
	id UUID) (map[string]bool, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT st.idempotency_key FROM sage.sre_steps st
		WHERE st.deployment_id = $1 AND st.database_id = $2 AND st.investigation_id = $3
		  AND EXISTS (SELECT 1 FROM sage.sre_evidence e
		              WHERE e.deployment_id = st.deployment_id
		                AND e.database_id = st.database_id
		                AND e.investigation_id = st.investigation_id
		                AND e.step_id = st.id)`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id))
	if err != nil {
		return nil, storeErr(ctx, "step keys", err)
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, storeErr(ctx, "step keys", err)
		}
		keys[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, storeErr(ctx, "step keys", err)
	}
	return keys, nil
}

// evidence reads an investigation's evidence, one row of it (only) or
// one step's rows (key).
func (s *PostgresStore) evidence(ctx context.Context, scope Scope, id UUID,
	only *UUID, key *string) ([]Evidence, error) {
	var filter *string
	if only != nil {
		v := string(*only)
		filter = &v
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id::text, e.step_id::text,
		    st.idempotency_key, e.probe_id, e.probe_version, e.capability_state,
		    COALESCE(e.reason_code, ''), e.observed_at, e.collected_at, e.payload::text,
		    e.sha256
		FROM sage.sre_evidence e
		JOIN sage.sre_steps st ON st.deployment_id = e.deployment_id
		 AND st.database_id = e.database_id AND st.investigation_id = e.investigation_id
		 AND st.id = e.step_id
		WHERE e.deployment_id = $1 AND e.database_id = $2 AND e.investigation_id = $3
		  AND ($4::uuid IS NULL OR e.id = $4::uuid)
		  AND ($5::text IS NULL OR st.idempotency_key = $5::text)
		ORDER BY st.sequence, e.observed_at, e.id`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), filter, key)
	if err != nil {
		return nil, storeErr(ctx, "evidence", err)
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, storeErr(ctx, "evidence", err)
		}
		out = append(out, e)
	}
	return out, storeErr(ctx, "evidence", rows.Err())
}

func scanEvidence(rows pgx.Rows) (Evidence, error) {
	var e Evidence
	var eid, sid, payload string
	var observed *time.Time
	if err := rows.Scan(&eid, &sid, &e.StepKey, &e.ProbeID, &e.ProbeVersion,
		&e.CapabilityState, &e.ReasonCode, &observed, &e.CollectedAt, &payload,
		&e.SHA256); err != nil {
		return e, err
	}
	e.ID, e.StepID, e.Payload = UUID(eid), UUID(sid), []byte(payload)
	e.ObservedAt = timeOrZero(observed)
	return e, nil
}

// VerifyHash recomputes the canonical payload hash.
func (e Evidence) VerifyHash() bool {
	canon, err := canonicalJSON(e.Payload)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(canon)
	return bytes.Equal(sum[:], e.SHA256)
}

func canonicalPayload(res probes.Result) ([]byte, error) {
	raw, err := res.Payload()
	if err != nil {
		return nil, err
	}
	return canonicalJSON(raw)
}

// canonicalJSON normalizes JSON so a payload hashes the same before and
// after a jsonb round trip: sorted keys, no whitespace, integers exact and
// other numbers in Go's shortest float form.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(canonicalValue(v))
}

func canonicalValue(v any) any {
	switch x := v.(type) {
	case map[string]any: // encoding/json writes map keys sorted
		for k := range x {
			x[k] = canonicalValue(x[k])
		}
		return x
	case []any:
		for i := range x {
			x[i] = canonicalValue(x[i])
		}
		return x
	case json.Number:
		// Integers stay exact (a queryid above 2^53 must not round); jsonb
		// keeps their digits. Other numbers take Go's shortest float form.
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return x
		}
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return x
	default:
		return v
	}
}
