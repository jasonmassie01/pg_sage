package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// setsLockKey serializes set writes with global and set ratifications
// (ratifyInTransaction locks 0 for a scope without a database).
const setsLockKey = 0

// PutSetRequest saves a set's selectors, priority and requirements; its
// document changes only through Propose and Ratify.
type PutSetRequest struct {
	Set   PolicySet
	Actor string
	// Dimensions are the review rubric's dimension ids (rubric.V1).
	Dimensions []string
}

// PutSet validates and saves a set, refusing one that would tie with
// another on a registered database. A new set starts with a copy of the
// default set's document (version 1), so it narrows or widens nothing
// until a proposal is ratified.
func (s *Store) PutSet(ctx context.Context, req PutSetRequest) (SetDocument, error) {
	if s == nil || s.pool == nil {
		return SetDocument{}, ErrUnavailable
	}
	if strings.TrimSpace(req.Actor) == "" {
		return SetDocument{}, invalidSet("the actor saving a set is required")
	}
	if err := ValidateSet(req.Set, req.Dimensions); err != nil {
		return SetDocument{}, err
	}
	var out SetDocument
	err := s.inLockedTx(ctx, setsLockKey, func(tx pgx.Tx) error {
		var err error
		out, err = putSetIn(ctx, tx, req, "")
		return err
	})
	return out, err
}

// putSetIn saves req.Set in tx, which holds the sets lock. managedBy ""
// keeps the row's owner (a new row is the API's); otherwise the row is
// marked with it (cli-02a).
func putSetIn(ctx context.Context, tx pgx.Tx, req PutSetRequest,
	managedBy string) (SetDocument, error) {
	existing, err := readSets(ctx, tx)
	if err != nil {
		return SetDocument{}, err
	}
	if err := checkCandidate(ctx, tx, existing, req.Set); err != nil {
		return SetDocument{}, err
	}
	if err := upsertSet(ctx, tx, req, managedBy); err != nil {
		return SetDocument{}, err
	}
	if err := ensureSetDocument(ctx, tx, existing, req.Set.Name, req.Actor); err != nil {
		return SetDocument{}, err
	}
	return savedSet(ctx, tx, req.Set.Name)
}

func checkCandidate(ctx context.Context, tx pgx.Tx, existing []SetDocument, set PolicySet) error {
	candidate := make([]PolicySet, 0, len(existing)+1)
	for _, e := range existing {
		if e.Set.Name != set.Name {
			candidate = append(candidate, e.Set)
		}
	}
	members, err := readMembers(ctx, tx, nil)
	if err != nil {
		return err
	}
	return CheckTies(append(candidate, set), members)
}

func upsertSet(ctx context.Context, tx pgx.Tx, req PutSetRequest, managedBy string) error {
	selectors, err := json.Marshal(req.Set.Selectors)
	if err != nil {
		return fmt.Errorf("encode selectors: %w", err)
	}
	if req.Set.Selectors == nil {
		selectors = []byte("[]")
	}
	requirements, err := json.Marshal(req.Set.Requirements)
	if err != nil {
		return fmt.Errorf("encode requirements: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO sage.policy_sets (name, priority, selectors, requirements, created_by, updated_by,
    managed_by)
VALUES ($1, $2, $3, $4, $5, $5, COALESCE(NULLIF($6, ''), 'api'))
ON CONFLICT (name) DO UPDATE SET priority = EXCLUDED.priority,
    selectors = EXCLUDED.selectors, requirements = EXCLUDED.requirements,
    updated_by = EXCLUDED.updated_by, updated_at = now(),
    managed_by = COALESCE(NULLIF($6, ''), sage.policy_sets.managed_by)`,
		req.Set.Name, req.Set.Priority, selectors, requirements, req.Actor,
		managedBy); err != nil {
		return fmt.Errorf("save policy set %s: %w", req.Set.Name, err)
	}
	detail, _ := json.Marshal(map[string]any{"priority": req.Set.Priority,
		"selectors": json.RawMessage(selectors), "requirements": json.RawMessage(requirements)})
	return recordSetEvent(ctx, tx, "set_saved", req.Set.Name, nil, req.Actor, detail)
}

// ensureSetDocument gives a set without a document a copy of default's.
func ensureSetDocument(
	ctx context.Context, tx pgx.Tx, existing []SetDocument, name, actor string,
) error {
	var base *Policy
	for _, e := range existing {
		if e.Set.Name == name && e.Policy != nil {
			return nil
		}
		if e.Set.Name == DefaultSetName {
			base = e.Policy
		}
	}
	if base == nil {
		return fmt.Errorf("%w: the default set has no document yet; start the sidecar "+
			"once so it bootstraps one, then save set %q", ErrNotFound, name)
	}
	return insertSetDocument(ctx, tx, name, base.Profile, base.Document, actor)
}

func insertSetDocument(
	ctx context.Context, tx pgx.Tx, name, profile string, doc json.RawMessage, actor string,
) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO sage.policy (database_id, version, scope, set_name, profile, doc, status,
    proposed_by, ratified_by, ratified_at, activated_at)
SELECT NULL, 1, $1, $2, $3, $4, 'active', $5, $5, now(), now()
WHERE NOT EXISTS (SELECT 1 FROM sage.policy
    WHERE set_name = $2 AND database_id IS NULL AND status = 'active')`,
		setScope(name), name, profile, doc, actor); err != nil {
		return fmt.Errorf("give policy set %s its first document: %w", name, err)
	}
	return nil
}

func savedSet(ctx context.Context, tx pgx.Tx, name string) (SetDocument, error) {
	sets, err := readSets(ctx, tx)
	if err != nil {
		return SetDocument{}, err
	}
	for _, s := range sets {
		if s.Set.Name == name {
			return s, nil
		}
	}
	return SetDocument{}, fmt.Errorf("%w: policy set %q after saving it", ErrNotFound, name)
}

// bootstrapSets gives every set without a document its first one: the
// profile's document for default, a copy of default's for the others.
// With no sets (or all bootstrapped) it writes nothing.
func (s *Store) bootstrapSets(ctx context.Context, profile, zone, actor string) error {
	sets, err := readSets(ctx, s.pool)
	if err != nil {
		return err
	}
	if !anyWithoutDocument(sets) {
		return nil
	}
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("bootstrap actor is required")
	}
	document, err := profileDocumentIn(profile, zone)
	if err != nil {
		return err
	}
	return s.inLockedTx(ctx, setsLockKey, func(tx pgx.Tx) error {
		current, err := readSets(ctx, tx)
		if err != nil {
			return err
		}
		base := &Policy{Profile: profile, Document: document}
		for _, set := range current {
			if set.Set.Name == DefaultSetName && set.Policy != nil {
				base = set.Policy
			}
		}
		for _, set := range current {
			if set.Policy != nil {
				continue
			}
			if err := insertSetDocument(ctx, tx, set.Set.Name, base.Profile, base.Document,
				actor); err != nil {
				return err
			}
		}
		return nil
	})
}

// inLockedTx runs fn in a transaction holding the advisory lock key.
func (s *Store) inLockedTx(ctx context.Context, key int64, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin policy set write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("lock policy scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit policy set write: %w", err)
	}
	return nil
}

func recordSetEvent(
	ctx context.Context, tx pgx.Tx, event, set string, databaseID *int64, actor string,
	detail []byte,
) error {
	if detail == nil {
		detail = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO sage.policy_set_events (event, set_name, database_id, actor, detail)
VALUES ($1, NULLIF($2, ''), $3, $4, $5)`, event, set, databaseID, actor, detail); err != nil {
		return fmt.Errorf("record policy set event %s: %w", event, err)
	}
	return nil
}

func anyWithoutDocument(sets []SetDocument) bool {
	for _, s := range sets {
		if s.Policy == nil {
			return true
		}
	}
	return false
}
