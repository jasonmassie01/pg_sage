package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The transaction surface of the GitOps directory reconciler (cli-02a):
// every call runs in the caller's transaction, so one reconcile of policy
// sets, principals and grants commits or rolls back whole. The caller
// takes LockSetsTx first; set writes and set ratifications then serialize
// with the API's (both take the same advisory lock).

// ManagedByConfig marks a set row the GitOps reconciler owns.
const ManagedByConfig = "config"

// LockSetsTx takes the sets lock for the rest of tx.
func LockSetsTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", setsLockKey); err != nil {
		return fmt.Errorf("lock policy sets: %w", err)
	}
	return nil
}

// SetsTx lists the sets with their active documents and owners.
func SetsTx(ctx context.Context, tx pgx.Tx) ([]SetDocument, error) {
	return readSets(ctx, tx)
}

// PutSetTx validates and saves req.Set in tx, marking the row managedBy.
func PutSetTx(ctx context.Context, tx pgx.Tx, req PutSetRequest,
	managedBy string) (SetDocument, error) {
	if strings.TrimSpace(req.Actor) == "" || strings.TrimSpace(managedBy) == "" {
		return SetDocument{}, invalidSet("the actor and owner saving a set are required")
	}
	if err := ValidateSet(req.Set, req.Dimensions); err != nil {
		return SetDocument{}, err
	}
	return putSetIn(ctx, tx, req, managedBy)
}

// DeleteSetTx removes a set the reconciler owns: its row, its active
// document (superseded) and its pending proposals (rejected), with a
// set_retired event. The default set is never removed, nor a set the API
// owns.
func DeleteSetTx(ctx context.Context, tx pgx.Tx, name, actor string) error {
	if name == DefaultSetName {
		return invalidSet("the default set cannot be removed")
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sage.policy_sets
WHERE name = $1 AND managed_by = 'config'`, name)
	if err != nil {
		return fmt.Errorf("remove policy set %s: %w", name, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: config-owned policy set %q", ErrNotFound, name)
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.policy
SET status = CASE WHEN status = 'active' THEN 'superseded' ELSE 'rejected' END
WHERE set_name = $1 AND database_id IS NULL AND status IN ('active', 'proposed')`,
		name); err != nil {
		return fmt.Errorf("retire the documents of policy set %s: %w", name, err)
	}
	return recordSetEvent(ctx, tx, "set_retired", name, nil, actor, nil)
}

// ProposeSetTx files doc as a proposal on set's document in force.
func ProposeSetTx(ctx context.Context, tx pgx.Tx, set, profile string,
	doc json.RawMessage, actor string) (Proposal, error) {
	current, err := setPolicy(ctx, tx, set)
	if err != nil {
		return Proposal{}, err
	}
	request := ProposalRequest{Scope: Scope{SetName: set}, ExpectedVersion: current.Version,
		Profile: profile, Document: doc, Actor: actor}
	if err := validateProposal(request); err != nil {
		return Proposal{}, err
	}
	preview, err := json.Marshal(request.Preview)
	if err != nil {
		return Proposal{}, fmt.Errorf("encode policy preview: %w", err)
	}
	return insertProposalIn(ctx, tx, request, current, preview)
}

func setPolicy(ctx context.Context, tx pgx.Tx, set string) (Policy, error) {
	sets, err := readSets(ctx, tx)
	if err != nil {
		return Policy{}, err
	}
	for _, s := range sets {
		if s.Set.Name == set && s.Policy != nil {
			return *s.Policy, nil
		}
	}
	return Policy{}, fmt.Errorf("%w: a document of policy set %q", ErrNotFound, set)
}

// RatifyTx ratifies a proposal in tx (Ratify without its own transaction).
func RatifyTx(ctx context.Context, tx pgx.Tx, request RatifyRequest,
	now time.Time) (Policy, error) {
	if request.ProposalID <= 0 || request.ExpectedVersion <= 0 ||
		strings.TrimSpace(request.Actor) == "" {
		return Policy{}, ErrInvalidDocument
	}
	return ratifyInTransaction(ctx, tx, request, now)
}

// PendingSetProposalsTx lists set's open proposals, oldest first.
func PendingSetProposalsTx(ctx context.Context, tx pgx.Tx, set string) ([]Proposal, error) {
	rows, err := tx.Query(ctx, `/* pg_sage policy_set_pending v1 */
SELECT id, profile, doc, proposed_by, proposed_at
FROM sage.policy
WHERE set_name = $1 AND database_id IS NULL AND status = 'proposed'
ORDER BY id`, set)
	if err != nil {
		return nil, fmt.Errorf("read pending proposals of policy set %s: %w", set, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Proposal, error) {
		p := Proposal{Scope: Scope{SetName: set}}
		err := row.Scan(&p.ID, &p.Profile, &p.Document, &p.Actor, &p.CreatedAt)
		return p, err
	})
}

// WideningMembersTx lists the databases whose document in force would
// widen if next were the sets (with their documents) instead of the
// stored ones. A database that resolves to nothing today (policy
// unavailable, which fails closed) and to a document under next counts
// as widened; one that would resolve to nothing does not.
func WideningMembersTx(ctx context.Context, tx pgx.Tx, next []SetDocument,
	now time.Time) ([]Member, error) {
	snap, err := loadSnapshot(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	var out []Member
	for _, m := range snap.members {
		after, err := Resolve(next, m, snap.own[m.DatabaseID],
			snap.exceptions[m.DatabaseID], now)
		if err != nil {
			continue
		}
		before, err := snap.resolve(m.DatabaseID, now)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrSetConflict) {
				return nil, err
			}
			out = append(out, m)
			continue
		}
		if widens, _ := WidensJSON(before.Policy.Document, after.Policy.Document); widens {
			out = append(out, m)
		}
	}
	return out, nil
}
