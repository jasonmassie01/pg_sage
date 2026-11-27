package sre

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Operator evidence refresh (playbooks-06). A finished investigation has
// no lease, so nothing else can add evidence to it; an operator who
// reviews it later re-reads one probe. The refresh is one step, one
// evidence row (source operator_refresh) and one evidence_refreshed event
// in a single transaction under the investigation row lock. The step's
// next state is the investigation's own terminal state: the run does not
// move, and its probe count and version are unchanged.

// EventEvidenceRefreshed is the event that attests to a refresh row.
const EventEvidenceRefreshed = "evidence_refreshed"

// RefreshKeyPrefix starts the idempotency key of every refresh step.
const RefreshKeyPrefix = "refresh:"

// ErrRefreshTooSoon refuses a refresh inside the request's MinInterval.
var ErrRefreshTooSoon = errors.New("evidence was refreshed too recently")

// RefreshRequest is one operator re-read of a finished investigation.
type RefreshRequest struct {
	// SourceEvidenceID is the evidence row the re-read repeats; Result
	// must come from the same probe.
	SourceEvidenceID UUID
	Result           probes.Result
	// MinInterval is the shortest gap between two refreshes of one
	// investigation, on the database clock; zero is no limit.
	MinInterval time.Duration
}

func (r RefreshRequest) validate(scope Scope, id UUID, actor string) error {
	if err := validateIDs(scope, id, r.SourceEvidenceID); err != nil {
		return err
	}
	if err := checkText("refresh actor", actor, true, 128); err != nil {
		return err
	}
	if r.MinInterval < 0 {
		return fmt.Errorf("%w: refresh interval %s is negative", ErrInvalidRequest,
			r.MinInterval)
	}
	if r.Result.ProbeID == "" || r.Result.Status == "" {
		return fmt.Errorf("%w: refresh needs a probe result", ErrInvalidRequest)
	}
	return nil
}

// AppendRefreshEvidence records an operator's re-read of one probe on a
// concluded or inconclusive investigation whose evidence is retained.
func (s *PostgresStore) AppendRefreshEvidence(ctx context.Context, scope Scope, id UUID,
	actor string, req RefreshRequest) (Evidence, error) {
	if err := req.validate(scope, id, actor); err != nil {
		return Evidence{}, err
	}
	var evidenceID UUID
	err := s.inTx(ctx, "refresh evidence", func(tx pgx.Tx) error {
		state, fence, err := lockRefreshable(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := checkRefreshSource(ctx, tx, scope, id, req); err != nil {
			return err
		}
		if err := checkRefreshInterval(ctx, tx, scope, id, req.MinInterval); err != nil {
			return err
		}
		evidenceID, err = insertRefresh(ctx, tx, scope, id, actor, state, fence, req)
		return err
	})
	if errors.Is(err, ErrRefreshTooSoon) { // a rate limit, not a metadata outage
		return Evidence{}, fmt.Errorf("%w: one refresh per investigation per %s",
			ErrRefreshTooSoon, req.MinInterval)
	}
	if err != nil {
		return Evidence{}, err
	}
	return s.EvidenceByID(ctx, scope, id, evidenceID)
}

// lockRefreshable locks a finished investigation that may be refreshed.
func lockRefreshable(ctx context.Context, tx pgx.Tx, scope Scope, id UUID) (State, int64,
	error) {
	var state string
	var fence int64
	var purged bool
	err := tx.QueryRow(ctx, `SELECT state, fence_token, evidence_purged_at IS NOT NULL
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).
		Scan(&state, &fence, &purged)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", 0, ErrNotFound
	case err != nil:
		return "", 0, err
	case State(state) != StateConcluded && State(state) != StateInconclusive:
		return "", 0, fmt.Errorf("%w: evidence refresh needs a concluded or inconclusive "+
			"investigation, not %s", ErrInvalidRequest, state)
	case purged:
		return "", 0, fmt.Errorf("%w: the investigation's evidence is purged",
			ErrInvalidRequest)
	case fence <= 0:
		return "", 0, fmt.Errorf("%w: the investigation never ran", ErrInvalidRequest)
	}
	return State(state), fence, nil
}

// checkRefreshSource requires the source row in this investigation, read
// by the same probe as the refresh.
func checkRefreshSource(ctx context.Context, tx pgx.Tx, scope Scope, id UUID,
	req RefreshRequest) error {
	var probeID string
	err := tx.QueryRow(ctx, `SELECT probe_id FROM sage.sre_evidence
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		  AND id = $4`, string(scope.DeploymentID), string(scope.DatabaseID), string(id),
		string(req.SourceEvidenceID)).Scan(&probeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: evidence %s", ErrNotFound, req.SourceEvidenceID)
	}
	if err != nil {
		return err
	}
	if probeID != string(req.Result.ProbeID) {
		return fmt.Errorf("%w: a %s refresh cannot repeat %s evidence", ErrInvalidRequest,
			req.Result.ProbeID, probeID)
	}
	return nil
}

// RefreshAllowed refuses, with ErrRefreshTooSoon, a refresh within
// interval of the investigation's last one. It takes no lock: a caller
// checks it before running the refresh probe, so a rate-limited call
// reads nothing on the monitored database; AppendRefreshEvidence checks
// again under the investigation lock.
func (s *PostgresStore) RefreshAllowed(ctx context.Context, scope Scope, id UUID,
	interval time.Duration) error {
	if err := validateIDs(scope, id); err != nil {
		return err
	}
	if interval < 0 {
		return fmt.Errorf("%w: refresh interval %s is negative", ErrInvalidRequest, interval)
	}
	err := checkRefreshInterval(ctx, s.pool, scope, id, interval)
	if err == nil || errors.Is(err, ErrRefreshTooSoon) {
		return err
	}
	return storeErr(ctx, "refresh interval", err)
}

// rowQuerier is a transaction or a pool.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// checkRefreshInterval refuses a refresh within interval of the last one.
func checkRefreshInterval(ctx context.Context, q rowQuerier, scope Scope, id UUID,
	interval time.Duration) error {
	if interval == 0 {
		return nil
	}
	var recent bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.sre_evidence
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		  AND source_kind = $4
		  AND collected_at > now() - make_interval(secs => $5))`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id),
		SourceOperatorRefresh, interval.Seconds()).Scan(&recent)
	if err != nil {
		return err
	}
	if recent {
		return fmt.Errorf("%w: one refresh per investigation per %s", ErrRefreshTooSoon,
			interval)
	}
	return nil
}

// insertRefresh writes the refresh step, its evidence row and the event
// that attests to both.
func insertRefresh(ctx context.Context, tx pgx.Tx, scope Scope, id UUID, actor string,
	state State, fence int64, req RefreshRequest) (UUID, error) {
	stepID := NewUUID()
	key := fmt.Sprintf("%s%s:%d", RefreshKeyPrefix, NewUUID(), time.Now().Unix()/60)
	_, err := tx.Exec(ctx, `INSERT INTO sage.sre_steps
		(deployment_id, database_id, investigation_id, id, sequence, idempotency_key,
		 fence_token, probe_count, next_state)
		SELECT $1, $2, $3, $4, COALESCE(max(sequence), 0) + 1, $5, $6, 1, $7
		FROM sage.sre_steps
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id), string(stepID),
		key, fence, string(state))
	if err != nil {
		return "", err
	}
	evidenceID, sum, err := insertEvidence(ctx, tx, scope, id, stepID,
		SourceOperatorRefresh, req.Result)
	if err != nil {
		return "", err
	}
	return evidenceID, appendEvent(ctx, tx, scope, id, EventEvidenceRefreshed, actor,
		map[string]any{"evidence_id": evidenceID, "source_evidence_id": req.SourceEvidenceID,
			"probe_id": req.Result.ProbeID, "sha256": hex.EncodeToString(sum),
			"step_key": key})
}

// unattestedRefreshes counts refresh rows no evidence_refreshed event
// names with the same id, hash and step: a row written or rewritten
// outside AppendRefreshEvidence.
func (s *PostgresStore) unattestedRefreshes(ctx context.Context, scope Scope,
	id UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*)
		FROM sage.sre_evidence e
		JOIN sage.sre_steps st ON st.deployment_id = e.deployment_id
		 AND st.database_id = e.database_id AND st.investigation_id = e.investigation_id
		 AND st.id = e.step_id
		WHERE e.deployment_id = $1 AND e.database_id = $2 AND e.investigation_id = $3
		  AND (e.source_kind = $4 OR starts_with(st.idempotency_key, $5))
		  AND NOT EXISTS (SELECT 1 FROM sage.sre_events ev
		      WHERE ev.deployment_id = e.deployment_id
		        AND ev.database_id = e.database_id
		        AND ev.investigation_id = e.investigation_id
		        AND ev.event_type = $6
		        AND ev.payload->>'evidence_id' = e.id::text
		        AND ev.payload->>'sha256' = encode(e.sha256, 'hex')
		        AND ev.payload->>'step_key' = st.idempotency_key)`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id),
		SourceOperatorRefresh, RefreshKeyPrefix, EventEvidenceRefreshed).Scan(&n)
	return n, err
}
