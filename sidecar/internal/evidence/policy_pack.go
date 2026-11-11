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
// name, read by (database scope, version) from the control database, with
// the SHA-256 of its stored document as doc_sha256, and the approvals of
// those versions as policy_approvals.json. The manifest's
// policy_hash_match is false when a recorded hash has no policy row or a
// different document; it is absent when no decision recorded a hash.
func (b *builder) policies(ctx context.Context, actionID int64) error {
	refs, err := b.policyRefs(ctx, actionID)
	if err != nil {
		return err
	}
	list, ids := []any{}, []int64{}
	seen := map[string]bool{}
	compared, match := false, true
	for _, ref := range refs {
		row, found, err := b.policyRow(ctx, ref)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%v/%d", scopeKey(ref.databaseID), ref.version)
		if found && !seen[key] {
			seen[key] = true
			list, ids = append(list, row.doc), append(ids, row.id)
		}
		if ref.sha256 != nil {
			compared = true
			match = match && found && row.sha256 == *ref.sha256
		}
	}
	if compared {
		b.pack.Manifest.PolicyHashMatch = &match
	}
	n := len(list)
	if err := b.add("policy.json", fileMeta{source: b.control.name, rows: &n},
		list); err != nil {
		return err
	}
	b.policyIDs = ids
	return b.policyApprovals(ctx, ids)
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
	rows, err := b.db.q.Query(ctx, `/* pg_sage evidence v1 */ SELECT DISTINCT d.database_id,
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

// storedPolicy is one stored policy version: its id, the row with
// doc_sha256, and that hash.
type storedPolicy struct {
	id     int64
	doc    json.RawMessage
	sha256 string
}

// policyRow reads one policy version from the control database and the
// hash of its stored document; found is false when it holds none.
func (b *builder) policyRow(ctx context.Context, ref policyRef) (storedPolicy, bool, error) {
	var row storedPolicy
	var exists bool
	if err := b.control.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT
		to_regclass('sage.policy') IS NOT NULL`).Scan(&exists); err != nil {
		return row, false, fmt.Errorf("evidence: policy.json from %s: %w",
			b.control.describe(), err)
	}
	if !exists {
		return row, false, nil
	}
	err := b.control.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT t.id,
		to_jsonb(t) || jsonb_build_object('doc_sha256', h.sha), h.sha FROM sage.policy t,
		LATERAL (SELECT encode(sha256(convert_to(t.doc::text, 'UTF8')), 'hex') AS sha) h
		WHERE t.database_id IS NOT DISTINCT FROM $1 AND t.version = $2`,
		ref.databaseID, ref.version).Scan(&row.id, &row.doc, &row.sha256)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, fmt.Errorf("evidence: policy.json: policy v%d of %s from %s: %w",
			ref.version, scopeKey(ref.databaseID), b.control.describe(), err)
	}
	return row, true, nil
}

// policyApprovals adds policy_approvals.json: each person's approval of
// the policy versions in policy.json, from the control database, in
// (policy, approver) order and at most maxWindowRows of them. It keeps
// their ids for the chain proof (nil when the table does not exist).
func (b *builder) policyApprovals(ctx context.Context, policyIDs []int64) error {
	meta := fileMeta{source: b.control.name}
	fail := func(err error) error {
		return fmt.Errorf("evidence: policy_approvals.json from %s: %w",
			b.control.describe(), err)
	}
	var exists bool
	if err := b.control.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT
		to_regclass('sage.policy_approvals') IS NOT NULL`).Scan(&exists); err != nil {
		return fail(err)
	}
	list := []any{}
	if exists {
		b.approvalIDs = []int64{}
	}
	if exists && len(policyIDs) > 0 {
		var raw []byte
		err := b.control.q.QueryRow(ctx, fmt.Sprintf(`/* pg_sage evidence v1 */ SELECT
			coalesce(jsonb_agg(to_jsonb(t) ORDER BY t.policy_id, t.approver), '[]'::jsonb),
			coalesce(array_agg(t.id ORDER BY t.id), '{}')
			FROM (SELECT * FROM sage.policy_approvals WHERE policy_id = ANY($1)
			ORDER BY policy_id, approver LIMIT %d) t`, maxWindowRows+1),
			policyIDs).Scan(&raw, &b.approvalIDs)
		if err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return fail(err)
		}
	}
	if len(list) > maxWindowRows {
		list, meta.truncated = list[:maxWindowRows], true
	}
	n := len(list)
	meta.rows = &n
	return b.add("policy_approvals.json", meta, list)
}
