package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// followUpTimeout bounds each follow-up statement.
const followUpTimeout = 5 * time.Second

// conclusionCandidatesSQL selects, per incident of the scope, the newest
// conclusion of the window. It is a candidate once it settled ($3 seconds),
// no run of the incident is queued or running and no follow-up of the
// incident was claimed. Newest first, at most $5.
const conclusionCandidatesSQL = `WITH newest AS (
    SELECT DISTINCT ON (i.source_incident_id) i.id, i.source_incident_id,
           i.source_case_id, i.trigger_kind, i.state, i.concluded_at,
           COALESCE(i.summary->>'family', '') AS family,
           COALESCE(i.summary->>'root', '') AS root,
           COALESCE(i.summary->>'reason', '') AS reason
      FROM sage.sre_investigations i
     WHERE i.deployment_id = $1 AND i.database_id = $2
       AND i.source_incident_id IS NOT NULL AND i.source_incident_id <> ''
       AND i.state IN ('concluded', 'inconclusive')
       AND i.concluded_at > now() - make_interval(secs => $4)
     ORDER BY i.source_incident_id, i.concluded_at DESC, i.id)
SELECT n.id::text, n.source_incident_id, n.source_case_id, n.trigger_kind, n.state,
       n.concluded_at, n.family, n.root, n.reason,
       COALESCE((SELECT h.label FROM sage.sre_hypotheses h
                  WHERE h.deployment_id = $1 AND h.database_id = $2
                    AND h.investigation_id = n.id AND h.status = 'root_cause'
                  ORDER BY h.revision DESC, h.ordinal LIMIT 1), '')
  FROM newest n
 WHERE n.concluded_at <= now() - make_interval(secs => $3)
   AND NOT EXISTS (SELECT 1 FROM sage.sre_incident_followup f
                    WHERE f.incident_id = n.source_incident_id)
   AND NOT EXISTS (SELECT 1 FROM sage.sre_investigations l
                    WHERE l.deployment_id = $1 AND l.database_id = $2
                      AND l.source_incident_id = n.source_incident_id
                      AND l.state IN ('queued', 'collecting', 'evaluating', 'needs_evidence'))
 ORDER BY n.concluded_at DESC
 LIMIT $5`

// ConclusionCandidates lists the scope's incidents whose newest settled
// conclusion still needs its follow-up.
func (s *PostgresStore) ConclusionCandidates(ctx context.Context, scope Scope,
	limit int) ([]ConclusionCandidate, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: candidate limit must be positive", ErrInvalidRequest)
	}
	qctx, cancel := context.WithTimeout(ctx, followUpTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, conclusionCandidatesSQL, string(scope.DeploymentID),
		string(scope.DatabaseID), ConclusionSettle.Seconds(), ConclusionWindow.Seconds(),
		limit)
	if err != nil {
		return nil, storeErr(ctx, "conclusion candidates", err)
	}
	defer rows.Close()
	var out []ConclusionCandidate
	for rows.Next() {
		var c ConclusionCandidate
		var id, kind, state string
		if err := rows.Scan(&id, &c.IncidentID, &c.CaseID, &kind, &state, &c.ConcludedAt,
			&c.Family, &c.Root, &c.Reason, &c.RootLabel); err != nil {
			return nil, storeErr(ctx, "conclusion candidates", err)
		}
		c.InvestigationID, c.Kind, c.State = UUID(id), TriggerKind(kind), State(state)
		out = append(out, c)
	}
	return out, storeErr(ctx, "conclusion candidates", rows.Err())
}

// ClaimFollowUp claims an incident's follow-up for investigation id;
// false when it was already claimed (by this or another sidecar).
func (s *PostgresStore) ClaimFollowUp(ctx context.Context, incidentID string,
	id UUID) (bool, error) {
	if strings.TrimSpace(incidentID) == "" {
		return false, fmt.Errorf("%w: follow-up claim needs an incident id", ErrInvalidRequest)
	}
	if _, err := ParseUUID(string(id)); err != nil {
		return false, err
	}
	qctx, cancel := context.WithTimeout(ctx, followUpTimeout)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `INSERT INTO sage.sre_incident_followup
		(incident_id, investigation_id) VALUES ($1, $2::uuid) ON CONFLICT DO NOTHING`,
		incidentID, string(id))
	if err != nil {
		return false, storeErr(ctx, "claim follow-up", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkFollowUpDelivered records that the claimed follow-up was delivered.
func (s *PostgresStore) MarkFollowUpDelivered(ctx context.Context, incidentID string,
	id UUID) error {
	qctx, cancel := context.WithTimeout(ctx, followUpTimeout)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `UPDATE sage.sre_incident_followup
		SET delivered_at = clock_timestamp()
		WHERE incident_id = $1 AND investigation_id = $2::uuid`, incidentID, string(id))
	if err != nil {
		return storeErr(ctx, "mark follow-up delivered", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no follow-up claim of incident %s by %s", ErrNotFound,
			incidentID, id)
	}
	return nil
}

// TrackedIncident reads an incident of this database from sage.incidents,
// resolved or not. An id that is not a UUID, or an incident of another
// database, is not tracked.
func (s *PGTriggerSource) TrackedIncident(ctx context.Context, id string) (TrackedIncident,
	bool, error) {
	if s == nil || s.pool == nil {
		return TrackedIncident{}, false, fmt.Errorf("%w: incident source has no database",
			ErrInvalidRequest)
	}
	if _, err := ParseUUID(id); err != nil {
		return TrackedIncident{}, false, nil
	}
	qctx, cancel := context.WithTimeout(ctx, followUpTimeout)
	defer cancel()
	var inc TrackedIncident
	err := s.pool.QueryRow(qctx, `SELECT severity, COALESCE(database_name, $2)
		FROM sage.incidents
		WHERE id = $1::uuid AND (database_name IS NULL OR database_name = $2)`,
		id, s.database).Scan(&inc.Severity, &inc.Database)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return TrackedIncident{}, false, nil
	case err != nil:
		return TrackedIncident{}, false, fmt.Errorf("read incident %s: %w", id, err)
	}
	return inc, true, nil
}
