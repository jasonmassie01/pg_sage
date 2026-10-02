package sre

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

var _ MemorySource = (*PostgresStore)(nil)

// SimilarInvestigations returns the finished investigations of the same
// database similar to q.Target, under the leakage guard: not the target,
// nothing created or concluded at or after the target's creation, not the
// same case, incident or trigger, and only outcomes recorded before the
// target's creation. Candidates share the trigger kind or the family.
func (s *PostgresStore) SimilarInvestigations(ctx context.Context, scope Scope,
	q SimilarQuery) ([]SimilarIncident, error) {
	t := q.Target
	if err := validateIDs(scope, t.ID); err != nil {
		return nil, err
	}
	if t.CreatedAt.IsZero() {
		return nil, fmt.Errorf("%w: the target's creation time is required",
			ErrInvalidRequest)
	}
	items, err := s.memoryCandidates(ctx, scope, q)
	if err != nil {
		return nil, storeErr(ctx, "similar investigations", err)
	}
	if err := s.memoryDetails(ctx, scope, t.CreatedAt, items); err != nil {
		return nil, storeErr(ctx, "similar investigation details", err)
	}
	return rankSimilar(items, q.Features, q.Limit), nil
}

func (s *PostgresStore) memoryCandidates(ctx context.Context, scope Scope,
	q SimilarQuery) ([]SimilarIncident, error) {
	t := q.Target
	fp := StartRequest{Scope: scope, TriggerKind: t.TriggerKind, Subject: t.Subject}
	rows, err := s.pool.Query(ctx, `SELECT id::text, source_case_id, trigger_kind, state,
		    summary::text, concluded_at
		FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id <> $3
		  AND state IN ('concluded', 'inconclusive')
		  AND created_at < $4 AND concluded_at < $4
		  AND (trigger_kind = $5 OR ($6 <> '' AND summary->>'family' = $6))
		  AND source_case_id <> $7
		  AND ($8 = '' OR COALESCE(source_incident_id, '') <> $8)
		  AND trigger_fingerprint <> $9
		ORDER BY concluded_at DESC, id LIMIT $10`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(t.ID), t.CreatedAt,
		string(t.TriggerKind), q.Family, t.CaseID, t.IncidentID, fp.Fingerprint(),
		memoryCandidates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SimilarIncident, error) {
		var it SimilarIncident
		var id, kind, state, summary string
		if err := r.Scan(&id, &it.CaseID, &kind, &state, &summary,
			&it.ConcludedAt); err != nil {
			return it, err
		}
		var sum Summary
		if err := json.Unmarshal([]byte(summary), &sum); err != nil {
			return it, fmt.Errorf("investigation %s summary: %w", id, err)
		}
		it.Label, it.InvestigationID = SimilarLabel, UUID(id)
		it.TriggerKind, it.State, it.Family = TriggerKind(kind), State(state), sum.Family
		it.features = recordFeatures(nil, sum) // missing probes; nodes added below
		return it, nil
	})
}

// memoryDetails adds each candidate's latest diagnosis revision and the
// latest outcome recorded before the cutoff.
func (s *PostgresStore) memoryDetails(ctx context.Context, scope Scope, cutoff time.Time,
	items []SimilarIncident) error {
	if len(items) == 0 {
		return nil
	}
	byID := map[string]*SimilarIncident{}
	ids := make([]string, 0, len(items))
	for i := range items {
		byID[string(items[i].InvestigationID)] = &items[i]
		ids = append(ids, string(items[i].InvestigationID))
	}
	if err := s.memoryHypotheses(ctx, scope, cutoff, ids, byID); err != nil {
		return err
	}
	return s.memoryOutcomes(ctx, scope, cutoff, ids, byID)
}

func (s *PostgresStore) memoryHypotheses(ctx context.Context, scope Scope, cutoff time.Time,
	ids []string, byID map[string]*SimilarIncident) error {
	rows, err := s.pool.Query(ctx, `SELECT h.investigation_id::text, h.node_id, h.status
		FROM sage.sre_hypotheses h
		WHERE h.deployment_id = $1 AND h.database_id = $2
		  AND h.investigation_id = ANY($3::uuid[]) AND h.created_at < $4
		  AND h.revision = (SELECT max(x.revision) FROM sage.sre_hypotheses x
		      WHERE x.deployment_id = h.deployment_id AND x.database_id = h.database_id
		        AND x.investigation_id = h.investigation_id AND x.created_at < $4)
		ORDER BY h.investigation_id, h.ordinal`,
		string(scope.DeploymentID), string(scope.DatabaseID), ids, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	records := map[string][]HypothesisRecord{}
	for rows.Next() {
		var id, node, status string
		if err := rows.Scan(&id, &node, &status); err != nil {
			return err
		}
		records[id] = append(records[id], HypothesisRecord{Node: node,
			Status: HypothesisStatus(status)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, it := range byID {
		it.describe(records[id])
	}
	return nil
}

// describe sets the incident's graph nodes from its latest revision and
// adds their features to the missing-probe features read from its summary.
func (it *SimilarIncident) describe(hs []HypothesisRecord) {
	it.Open, it.RuledOut = []string{}, []string{}
	for _, h := range hs {
		switch h.Status {
		case HypothesisRoot:
			it.Root = h.Node
			it.Open = append(it.Open, h.Node)
		case HypothesisRuledOut:
			it.RuledOut = append(it.RuledOut, h.Node)
		default:
			it.Open = append(it.Open, h.Node)
		}
	}
	f := featureSet{}
	for _, x := range append(it.features, recordFeatures(hs, Summary{})...) {
		f[x] = true
	}
	it.features = f.sorted()
}

func (s *PostgresStore) memoryOutcomes(ctx context.Context, scope Scope, cutoff time.Time,
	ids []string, byID map[string]*SimilarIncident) error {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (investigation_id)
		    investigation_id::text, verdict, COALESCE(actual_node, ''), actor, recorded_at
		FROM sage.sre_investigation_outcomes
		WHERE deployment_id = $1 AND database_id = $2
		  AND investigation_id = ANY($3::uuid[]) AND recorded_at < $4
		ORDER BY investigation_id, recorded_at DESC`,
		string(scope.DeploymentID), string(scope.DatabaseID), ids, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, verdict string
		var o Outcome
		if err := rows.Scan(&id, &verdict, &o.ActualNode, &o.Actor,
			&o.RecordedAt); err != nil {
			return err
		}
		o.Verdict = OutcomeVerdict(verdict)
		if it := byID[id]; it != nil {
			it.Outcome = &o
		}
	}
	return rows.Err()
}

// RecordOutcome records an operator's verdict on a finished investigation:
// confirmed (its root cause, or the named actual node of an inconclusive
// one) or refuted (optionally naming the actual node).
func (s *PostgresStore) RecordOutcome(ctx context.Context, scope Scope, id UUID,
	req OutcomeRequest) (Outcome, error) {
	if err := validateIDs(scope, id); err != nil {
		return Outcome{}, err
	}
	if err := req.validate(); err != nil {
		return Outcome{}, err
	}
	inv, err := s.Get(ctx, scope, id)
	if err != nil {
		return Outcome{}, err
	}
	switch {
	case inv.State != StateConcluded && inv.State != StateInconclusive:
		return Outcome{}, fmt.Errorf("%w: only a concluded or inconclusive "+
			"investigation has an outcome (state %s)", ErrInvalidRequest, inv.State)
	case req.Verdict == string(OutcomeConfirmed) && inv.Summary.Root == "" &&
		req.ActualNode == "":
		return Outcome{}, fmt.Errorf("%w: confirming an investigation without a root "+
			"cause needs the actual node", ErrInvalidRequest)
	}
	o := Outcome{Verdict: OutcomeVerdict(req.Verdict), ActualNode: req.ActualNode,
		Actor: req.Actor}
	err = s.pool.QueryRow(ctx, `INSERT INTO sage.sre_investigation_outcomes
		(deployment_id, database_id, investigation_id, id, verdict, actual_node, actor)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7) RETURNING recorded_at`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id),
		string(NewUUID()), req.Verdict, req.ActualNode, req.Actor).Scan(&o.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	}
	return o, storeErr(ctx, "record outcome", err)
}

func (r OutcomeRequest) validate() error {
	if r.Verdict != string(OutcomeConfirmed) && r.Verdict != string(OutcomeRefuted) {
		return fmt.Errorf("%w: verdict must be confirmed or refuted", ErrInvalidRequest)
	}
	if r.ActualNode != "" {
		if _, ok := causal.NodeByID(causal.NodeID(r.ActualNode)); !ok {
			return fmt.Errorf("%w: %q is not a causal graph node", ErrInvalidRequest,
				r.ActualNode)
		}
	}
	return checkText("actor", r.Actor, true, 128)
}
