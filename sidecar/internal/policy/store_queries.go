package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const currentPolicySQL = `
SELECT id, database_id, version, profile, doc, ratified_by, activated_at
FROM sage.policy
WHERE database_id IS NOT DISTINCT FROM $1 AND status = 'active'
ORDER BY version DESC LIMIT 1`

const policyHistorySQL = `
SELECT id, database_id, version, profile, doc,
       COALESCE(ratified_by, proposed_by),
       COALESCE(activated_at, ratified_at, proposed_at)
FROM sage.policy
WHERE database_id IS NOT DISTINCT FROM $1
  AND status IN ('active', 'superseded')
ORDER BY version DESC LIMIT $2`

type rowScanner interface {
	Scan(...any) error
}

func scanPolicy(row rowScanner) (Policy, error) {
	var item Policy
	var databaseID *int64
	err := row.Scan(
		&item.ID, &databaseID, &item.Version, &item.Profile,
		&item.Document, &item.UpdatedBy, &item.UpdatedAt,
	)
	item.Scope.DatabaseID = databaseID
	return item, err
}

func (s *Store) insertProposal(
	ctx context.Context, request ProposalRequest, current Policy, preview []byte,
) (Proposal, error) {
	var result Proposal
	var databaseID *int64
	err := s.pool.QueryRow(ctx, `
WITH next_id AS (SELECT nextval('sage.policy_id_seq') AS id)
INSERT INTO sage.policy (
    id, database_id, version, scope, profile, doc, preview, status,
    proposed_by, supersedes_id
)
SELECT id, $1, -id, $2, $3, $4, $5, 'proposed', $6, $7 FROM next_id
RETURNING id, database_id, profile, doc, proposed_by, preview, proposed_at`,
		request.Scope.DatabaseID, scopeName(request.Scope), request.Profile,
		request.Document, preview, request.Actor, current.ID,
	).Scan(
		&result.ID, &databaseID, &result.Profile, &result.Document,
		&result.Actor, &result.Preview, &result.CreatedAt,
	)
	if err != nil {
		return Proposal{}, fmt.Errorf("insert policy proposal: %w", err)
	}
	result.Scope.DatabaseID = databaseID
	result.BaseVersion = current.Version
	return result, nil
}

func ratifyInTransaction(
	ctx context.Context, tx pgx.Tx, request RatifyRequest,
) (Policy, error) {
	proposal, baseID, err := loadProposalForUpdate(ctx, tx, request.ProposalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, fmt.Errorf("load policy proposal: %w", err)
	}
	lockKey := int64(0)
	if proposal.Scope.DatabaseID != nil {
		lockKey = *proposal.Scope.DatabaseID
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		return Policy{}, fmt.Errorf("lock policy scope: %w", err)
	}
	current, err := currentPolicyForUpdate(ctx, tx, proposal.Scope)
	if err != nil {
		return Policy{}, err
	}
	if current.ID != baseID || current.Version != request.ExpectedVersion {
		return Policy{}, ErrVersionConflict
	}
	if _, err := tx.Exec(ctx, `
UPDATE sage.policy SET status = 'superseded' WHERE id = $1`, current.ID); err != nil {
		return Policy{}, fmt.Errorf("supersede policy: %w", err)
	}
	return activateProposal(ctx, tx, proposal, current, request.Actor)
}

func loadProposalForUpdate(
	ctx context.Context, tx pgx.Tx, proposalID int64,
) (Proposal, int64, error) {
	var result Proposal
	var databaseID *int64
	var baseID int64
	err := tx.QueryRow(ctx, `
SELECT id, database_id, profile, doc, proposed_by, preview, proposed_at,
       supersedes_id
FROM sage.policy WHERE id = $1 AND status = 'proposed' FOR UPDATE`, proposalID).Scan(
		&result.ID, &databaseID, &result.Profile, &result.Document,
		&result.Actor, &result.Preview, &result.CreatedAt, &baseID,
	)
	result.Scope.DatabaseID = databaseID
	return result, baseID, err
}

func currentPolicyForUpdate(
	ctx context.Context, tx pgx.Tx, scope Scope,
) (Policy, error) {
	row := tx.QueryRow(ctx, currentPolicySQL+" FOR UPDATE", scope.DatabaseID)
	policy, err := scanPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	return policy, err
}

func activateProposal(
	ctx context.Context, tx pgx.Tx, proposal Proposal, current Policy, actor string,
) (Policy, error) {
	row := tx.QueryRow(ctx, `
UPDATE sage.policy
SET version = $2, status = 'active', ratified_by = $3,
    ratified_at = now(), activated_at = now()
WHERE id = $1
RETURNING id, database_id, version, profile, doc, ratified_by, activated_at`,
		proposal.ID, current.Version+1, actor,
	)
	policy, err := scanPolicy(row)
	if err != nil {
		return Policy{}, fmt.Errorf("activate policy proposal: %w", err)
	}
	return policy, nil
}

func validateProposal(request ProposalRequest) error {
	if request.ExpectedVersion <= 0 || strings.TrimSpace(request.Actor) == "" ||
		(request.Profile != "staffed" && request.Profile != "unattended") {
		return ErrInvalidDocument
	}
	if _, err := ParseDocument(request.Document); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	if _, err := json.Marshal(request.Preview); err != nil {
		return fmt.Errorf("%w: preview: %v", ErrInvalidDocument, err)
	}
	return nil
}

func profileDocument(profile string) ([]byte, error) {
	switch profile {
	case "staffed":
		return MarshalDocument(StaffedProfile())
	case "unattended":
		return MarshalDocument(UnattendedProfile())
	default:
		return nil, ErrInvalidDocument
	}
}

func scopeName(scope Scope) string {
	if scope.DatabaseID == nil {
		return "global"
	}
	return "database"
}
