package sre

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Conclude persists a diagnosis under the lease and ends the run: the
// lease, fence and segment deadline are checked in the same transaction
// that inserts the hypotheses (a new revision), writes the summary,
// charges active time, clears the lease and appends the event. Every
// evidence id the conclusion cites must belong to this investigation.
func (s *PostgresStore) Conclude(ctx context.Context, lease Lease,
	c Conclusion) (Investigation, error) {
	if err := lease.validate(); err != nil {
		return Investigation{}, err
	}
	if err := c.validate(); err != nil {
		return Investigation{}, err
	}
	var inv Investigation
	err := s.inTx(ctx, "conclude", func(tx pgx.Tx) error {
		state, _, err := s.lockLeased(ctx, tx, lease)
		if err != nil {
			return err
		}
		if !CanTransition(state, c.State) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, state, c.State)
		}
		if err := s.checkEvidenceScope(ctx, tx, lease, c.evidenceRefs()); err != nil {
			return err
		}
		revision, err := insertHypotheses(ctx, tx, lease, c.Hypotheses)
		if err != nil {
			return err
		}
		inv, err = s.finishRun(ctx, tx, lease, c)
		if errors.Is(err, ErrNotFound) {
			return ErrLeaseLost // the lease expired inside the transaction
		}
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, lease.Scope, lease.InvestigationID, EventConcluded,
			workerActor(lease), map[string]any{"state": c.State, "root": c.Summary.Root,
				"revision": revision, "hypotheses": len(c.Hypotheses),
				"failure_code": c.FailureCode})
	})
	return inv, err
}

// checkEvidenceScope rejects references to evidence outside the
// investigation (another investigation, database or deployment).
func (s *PostgresStore) checkEvidenceScope(ctx context.Context, tx pgx.Tx, lease Lease,
	refs []UUID) error {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, string(r))
	}
	var missing int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM unnest($4::uuid[]) AS r(id)
		WHERE NOT EXISTS (SELECT 1 FROM sage.sre_evidence e
		    WHERE e.deployment_id = $1 AND e.database_id = $2
		      AND e.investigation_id = $3 AND e.id = r.id)`,
		string(lease.Scope.DeploymentID), string(lease.Scope.DatabaseID),
		string(lease.InvestigationID), ids).Scan(&missing)
	if err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%w: %d evidence references are not in this investigation",
			ErrInvalidRequest, missing)
	}
	return nil
}

func insertHypotheses(ctx context.Context, tx pgx.Tx, lease Lease,
	hs []HypothesisRecord) (int, error) {
	sc := lease.Scope
	var revision int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) + 1
		FROM sage.sre_hypotheses
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3`,
		string(sc.DeploymentID), string(sc.DatabaseID),
		string(lease.InvestigationID)).Scan(&revision); err != nil {
		return 0, err
	}
	for i, h := range hs {
		support, _ := json.Marshal(nonNilFacts(h.Support))
		contradict, _ := json.Marshal(nonNilFacts(h.Contradict))
		if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_hypotheses
			(deployment_id, database_id, investigation_id, id, revision, ordinal,
			 graph_version, family, node_id, label, mechanism, subject, status,
			 confidence, supporting_ids, contradicting_ids, support, contradict,
			 refutation_probe, operator_step)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			        $15::uuid[], $16::uuid[], $17, $18, $19, $20)`,
			string(sc.DeploymentID), string(sc.DatabaseID), string(lease.InvestigationID),
			string(NewUUID()), revision, i+1, h.GraphVersion, h.Family, h.Node, h.Label,
			h.Mechanism, h.Subject, string(h.Status), h.Confidence, factIDs(h.Support),
			factIDs(h.Contradict), string(support), string(contradict),
			h.RefutationProbe, h.OperatorStep); err != nil {
			return 0, err
		}
	}
	return revision, nil
}

func nonNilFacts(f []Fact) []Fact {
	if f == nil {
		return []Fact{}
	}
	return f
}

func (s *PostgresStore) finishRun(ctx context.Context, tx pgx.Tx, lease Lease,
	c Conclusion) (Investigation, error) {
	summary, _ := json.Marshal(c.Summary)
	return scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
		SET state = $6, summary = $7::jsonb, failure_code = NULLIF($8, ''),
		    active_ms = `+fmt.Sprintf(chargeSQL, 9)+`,
		    concluded_at = clock_timestamp(),
		    lease_owner = NULL, lease_started_at = NULL, lease_until = NULL,
		    version = version + 1, updated_at = clock_timestamp()
		WHERE `+leaseGuard+` RETURNING `+invColumns,
		append(leaseArgs(lease), string(c.State), string(summary), c.FailureCode,
			s.limits.MaxActive.Milliseconds())...))
}

// Hypotheses lists an investigation's hypotheses, newest revision first
// and in diagnosis order within a revision.
func (s *PostgresStore) Hypotheses(ctx context.Context, scope Scope,
	id UUID) ([]HypothesisRecord, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, revision, ordinal, graph_version,
		    family, node_id, label, mechanism, subject, status, confidence,
		    support::text, contradict::text, refutation_probe, operator_step, created_at
		FROM sage.sre_hypotheses
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		ORDER BY revision DESC, ordinal`, string(scope.DeploymentID),
		string(scope.DatabaseID), string(id))
	if err != nil {
		return nil, storeErr(ctx, "hypotheses", err)
	}
	defer rows.Close()
	var out []HypothesisRecord
	for rows.Next() {
		h, err := scanHypothesis(rows)
		if err != nil {
			return nil, storeErr(ctx, "hypotheses", err)
		}
		out = append(out, h)
	}
	return out, storeErr(ctx, "hypotheses", rows.Err())
}

func scanHypothesis(rows pgx.Rows) (HypothesisRecord, error) {
	var h HypothesisRecord
	var id, status, support, contradict string
	if err := rows.Scan(&id, &h.Revision, &h.Ordinal, &h.GraphVersion, &h.Family,
		&h.Node, &h.Label, &h.Mechanism, &h.Subject, &status, &h.Confidence, &support,
		&contradict, &h.RefutationProbe, &h.OperatorStep, &h.CreatedAt); err != nil {
		return h, err
	}
	h.ID, h.Status = UUID(id), HypothesisStatus(status)
	if err := json.Unmarshal([]byte(support), &h.Support); err != nil {
		return h, err
	}
	return h, json.Unmarshal([]byte(contradict), &h.Contradict)
}

// SetPinned pins (retention keeps it) or unpins an investigation. A
// change is recorded in the event chain with the actor; setting the
// current value again changes nothing.
func (s *PostgresStore) SetPinned(ctx context.Context, scope Scope, id UUID,
	pinned bool, actor string) (Investigation, error) {
	if err := validateIDs(scope, id); err != nil {
		return Investigation{}, err
	}
	if err := checkText("actor", actor, true, 128); err != nil {
		return Investigation{}, err
	}
	var inv Investigation
	err := s.inTx(ctx, "pin", func(tx pgx.Tx) error {
		var current bool
		err := tx.QueryRow(ctx, `SELECT pinned FROM sage.sre_investigations
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id)).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		inv, err = scanInvestigation(tx.QueryRow(ctx, `UPDATE sage.sre_investigations
			SET pinned = $4, version = version + CASE WHEN pinned = $4 THEN 0 ELSE 1 END,
			    updated_at = CASE WHEN pinned = $4 THEN updated_at
			                      ELSE clock_timestamp() END
			WHERE deployment_id = $1 AND database_id = $2 AND id = $3
			RETURNING `+invColumns, string(scope.DeploymentID),
			string(scope.DatabaseID), string(id), pinned))
		if err != nil || current == pinned {
			return err
		}
		typ := EventUnpinned
		if pinned {
			typ = EventPinned
		}
		return appendEvent(ctx, tx, scope, id, typ, actor, nil)
	})
	return inv, err
}

// EvidenceByID returns one evidence row of one investigation; an id of
// another investigation or scope is ErrNotFound.
func (s *PostgresStore) EvidenceByID(ctx context.Context, scope Scope,
	id, evidenceID UUID) (Evidence, error) {
	if err := validateIDs(scope, id, evidenceID); err != nil {
		return Evidence{}, err
	}
	all, err := s.evidence(ctx, scope, id, &evidenceID)
	if err != nil {
		return Evidence{}, err
	}
	if len(all) == 0 {
		return Evidence{}, ErrNotFound
	}
	return all[0], nil
}

// Pending lists, oldest first, investigations a worker should run:
// queued, waiting for evidence, or active with an expired lease.
func (s *PostgresStore) Pending(ctx context.Context, scope Scope, limit int) ([]UUID, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit %d outside [1, 1000]", ErrInvalidRequest, limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2
		  AND (state IN ('queued', 'needs_evidence')
		       OR (state IN ('collecting', 'evaluating')
		           AND COALESCE(lease_until <= clock_timestamp(), true)))
		ORDER BY created_at, id LIMIT $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), limit)
	if err != nil {
		return nil, storeErr(ctx, "pending", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, storeErr(ctx, "pending", err)
	}
	out := make([]UUID, 0, len(ids))
	for _, id := range ids {
		out = append(out, UUID(id))
	}
	return out, nil
}

// cursorTime formats a list cursor's time part.
func cursorTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
