package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Store reads behind the replay-case export (roadmap 2.4): an
// investigation's newest operator outcome, and the scope of an
// investigation known only by its id (the CLI reads the control database
// directly, without a bound coordinator).

// LatestOutcome is the investigation's newest operator outcome, or nil.
func (s *PostgresStore) LatestOutcome(ctx context.Context, scope Scope, id UUID) (*Outcome,
	error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	var o Outcome
	var verdict string
	err := s.pool.QueryRow(ctx, `SELECT verdict, COALESCE(actual_node, ''), actor,
		recorded_at FROM sage.sre_investigation_outcomes
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		ORDER BY recorded_at DESC LIMIT 1`, string(scope.DeploymentID),
		string(scope.DatabaseID), string(id)).Scan(&verdict, &o.ActualNode, &o.Actor,
		&o.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, storeErr(ctx, "latest outcome", err)
	}
	o.Verdict = OutcomeVerdict(verdict)
	return &o, nil
}

// LookupScope is the scope of the investigation with id.
func (s *PostgresStore) LookupScope(ctx context.Context, id UUID) (Scope, error) {
	if _, err := ParseUUID(string(id)); err != nil {
		return Scope{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT deployment_id::text, database_id::text
		FROM sage.sre_investigations WHERE id = $1 LIMIT 2`, string(id))
	if err != nil {
		return Scope{}, storeErr(ctx, "lookup investigation scope", err)
	}
	defer rows.Close()
	var found []Scope
	for rows.Next() {
		var d, db string
		if err := rows.Scan(&d, &db); err != nil {
			return Scope{}, storeErr(ctx, "scan investigation scope", err)
		}
		found = append(found, Scope{DeploymentID: UUID(d), DatabaseID: UUID(db)})
	}
	if err := rows.Err(); err != nil {
		return Scope{}, storeErr(ctx, "lookup investigation scope", err)
	}
	switch len(found) {
	case 0:
		return Scope{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	case 1:
		return found[0], nil
	}
	return Scope{}, fmt.Errorf("%w: investigation %s exists in more than one database",
		ErrInvalidRequest, id)
}
