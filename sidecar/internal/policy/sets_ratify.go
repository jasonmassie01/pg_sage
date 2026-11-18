package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ratifyDatabaseProposal activates a proposal on a database's scope. Its
// base is the document in force (Resolve): the database's own, or its
// set's. Ratified, it is the database's own document, an exception against
// the set it matches; the set's document is never touched.
func ratifyDatabaseProposal(
	ctx context.Context, tx pgx.Tx, proposal Proposal, baseID int64, request RatifyRequest,
	now time.Time,
) (Policy, error) {
	id := *proposal.Scope.DatabaseID
	snap, err := loadSnapshot(ctx, tx, &id)
	if err != nil {
		return Policy{}, err
	}
	res, err := snap.resolve(id, now)
	if err != nil {
		return Policy{}, err
	}
	if err := lockActiveRow(ctx, tx, res.Policy.ID); err != nil {
		return Policy{}, err
	}
	if res.Policy.ID != baseID || res.Policy.Version != request.ExpectedVersion {
		return Policy{}, ErrVersionConflict
	}
	if err := approveProposal(ctx, tx, proposal, request); err != nil {
		return Policy{}, err
	}
	if err := retireOwnDocument(ctx, tx, id, res); err != nil {
		return Policy{}, err
	}
	version, err := nextDatabaseVersion(ctx, tx, id, res.Policy.Version)
	if err != nil {
		return Policy{}, err
	}
	policy, err := activateProposal(ctx, tx, proposal, version, request.Actor)
	if err != nil || res.Set == "" {
		return policy, err
	}
	return policy, adoptException(ctx, tx, id, res.Set, request.Actor, proposal.ID)
}

// lockActiveRow locks the base row; a row superseded meanwhile is a
// version conflict.
func lockActiveRow(ctx context.Context, tx pgx.Tx, id int64) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM sage.policy WHERE id = $1 AND status = 'active'
FOR UPDATE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrVersionConflict
	}
	if err != nil {
		return fmt.Errorf("lock policy %d: %w", id, err)
	}
	return nil
}

// retireOwnDocument supersedes the database's own active document: an
// expired exception through the expiry path (with its audit row).
func retireOwnDocument(ctx context.Context, tx pgx.Tx, id int64, res Resolution) error {
	if res.ExceptionExpired {
		return expireException(ctx, tx, id, res.Set)
	}
	if res.Exception == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.policy SET status = 'superseded' WHERE id = $1`,
		res.Policy.ID); err != nil {
		return fmt.Errorf("supersede policy: %w", err)
	}
	return nil
}

// nextDatabaseVersion is above both the database's own versions and the
// version it saw last, so the versions a database reports only grow.
func nextDatabaseVersion(ctx context.Context, tx pgx.Tx, id, seen int64) (int64, error) {
	var own int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM sage.policy
WHERE database_id = $1 AND version > 0`, id).Scan(&own); err != nil {
		return 0, fmt.Errorf("read database %d's policy versions: %w", id, err)
	}
	return max(own, seen) + 1, nil
}

// adoptException records the exception metadata of a database's newly
// ratified own document, keeping metadata (and expiry) already there.
func adoptException(
	ctx context.Context, tx pgx.Tx, id int64, set, actor string, proposalID int64,
) error {
	tag, err := tx.Exec(ctx, `
INSERT INTO sage.policy_exceptions (database_id, set_name, set_by, reason)
VALUES ($1, $2, $3, $4) ON CONFLICT (database_id) DO NOTHING`, id, set, actor,
		fmt.Sprintf("ratified per-database policy proposal %d", proposalID))
	if err != nil {
		return fmt.Errorf("record exception for database %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	detail, _ := json.Marshal(map[string]int64{"proposal_id": proposalID})
	return recordSetEvent(ctx, tx, "exception_set", set, &id, actor, detail)
}
