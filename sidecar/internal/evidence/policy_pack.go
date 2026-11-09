package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// policyRef is the standing policy one of the action's decisions names:
// its database scope, version and the document hash the gate recorded
// (nil on a decision written before policy-03).
type policyRef struct {
	databaseID *int64
	version    int64
	sha256     *string
}

// policies adds policy.json: each policy version the action's decisions
// name, read by (database scope, version) from the policy database, with
// the SHA-256 of its stored document as doc_sha256. The manifest's
// policy_hash_match is false when a recorded hash has no policy row or a
// different document; it is absent when no decision recorded a hash.
func (b *builder) policies(ctx context.Context, actionID int64) error {
	refs, err := b.policyRefs(ctx, actionID)
	if err != nil {
		return err
	}
	list := []any{}
	seen := map[string]bool{}
	compared, match := false, true
	for _, ref := range refs {
		row, hash, found, err := b.policyRow(ctx, ref)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%v/%d", scopeKey(ref.databaseID), ref.version)
		if found && !seen[key] {
			seen[key] = true
			list = append(list, row)
		}
		if ref.sha256 != nil {
			compared = true
			match = match && found && hash == *ref.sha256
		}
	}
	if compared {
		b.pack.Manifest.PolicyHashMatch = &match
	}
	return b.add("policy.json", list)
}

func scopeKey(databaseID *int64) string {
	if databaseID == nil {
		return "global"
	}
	return fmt.Sprint(*databaseID)
}

// policyRefs lists the policies the action's decisions name. A decision
// made before any policy read (version 0) names none.
func (b *builder) policyRefs(ctx context.Context, actionID int64) ([]policyRef, error) {
	rows, err := b.q.Query(ctx, `/* pg_sage evidence v1 */ SELECT DISTINCT d.database_id,
		d.policy_version, d.policy_doc_sha256 FROM sage.decision d
		WHERE (`+decisionFilter("d")+`) AND d.policy_version > 0
		ORDER BY 1, 2, 3`, actionID)
	if err != nil {
		return nil, fmt.Errorf("evidence: policy.json: decisions of action %d: %w",
			actionID, err)
	}
	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (policyRef, error) {
		var ref policyRef
		return ref, row.Scan(&ref.databaseID, &ref.version, &ref.sha256)
	})
	if err != nil {
		return nil, fmt.Errorf("evidence: policy.json: decisions of action %d: %w",
			actionID, err)
	}
	return refs, nil
}

// policyRow reads one policy version from the policy database and the
// hash of its stored document; found is false when it holds none.
func (b *builder) policyRow(
	ctx context.Context, ref policyRef,
) (json.RawMessage, string, bool, error) {
	var exists bool
	if err := b.policyQ.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT
		to_regclass('sage.policy') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, "", false, fmt.Errorf("evidence: policy.json: %w", err)
	}
	if !exists {
		return nil, "", false, nil
	}
	var raw json.RawMessage
	var hash string
	err := b.policyQ.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT to_jsonb(t) ||
		jsonb_build_object('doc_sha256', h.sha), h.sha FROM sage.policy t,
		LATERAL (SELECT encode(sha256(convert_to(t.doc::text, 'UTF8')), 'hex') AS sha) h
		WHERE t.database_id IS NOT DISTINCT FROM $1 AND t.version = $2`,
		ref.databaseID, ref.version).Scan(&raw, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("evidence: policy.json: policy v%d of %s: %w",
			ref.version, scopeKey(ref.databaseID), err)
	}
	return raw, hash, true, nil
}
