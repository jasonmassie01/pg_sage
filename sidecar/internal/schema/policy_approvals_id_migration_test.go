package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// audit-11: sage.policy_approvals had a composite key and no id, and the
// audit chain keys every link on the row's id. The step adds an identity
// id (the composite key stays), after which the chain installs and marks
// the rows written before it as legacy.

// No concurrent access tests here: the step runs under the bootstrap lock,
// which bootstrap_lock_test.go covers.

func TestPolicyApprovalsIDStepIsGuarded(t *testing.T) {
	for _, want := range []string{"lock_timeout", "to_regclass('sage.policy_approvals')",
		"GENERATED ALWAYS AS IDENTITY", "UNIQUE", "attname = 'id'"} {
		if !strings.Contains(ddlPolicyApprovalsID, want) {
			t.Errorf("ddlPolicyApprovalsID lacks %q", want)
		}
	}
}

// Integration: an install from before audit-11 (approvals with no id and
// no chain) gains the id and the chain; its old approvals are legacy, a new
// one is chained, and further bootstraps change nothing.
func TestPolicyApprovalsIDMigrationUpgradesAnOldInstall(t *testing.T) {
	pool := newPool(t, "schema_policy_approvals_id")
	ctx := context.Background()
	bootstrapWithRetry(t, ctx, pool)
	for _, sql := range []string{
		`DROP TRIGGER audit_chain_v1_id ON sage.policy_approvals`,
		`DROP TRIGGER audit_chain_v1_u ON sage.policy_approvals`,
		`DROP TRIGGER audit_chain_v1_t ON sage.policy_approvals`,
		`ALTER TABLE sage.policy_approvals DROP COLUMN id, DROP COLUMN chain_seq,
			DROP COLUMN chain_prev_hash, DROP COLUMN chain_hash`,
		`DELETE FROM sage.audit_chain_meta WHERE chain = 'policy_approvals'`,
		`INSERT INTO sage.policy (database_id, version, doc, proposed_by)
			VALUES (41, 1, '{}', 'agent'), (41, 2, '{}', 'agent')`,
		`INSERT INTO sage.policy_approvals (policy_id, approver, approver_user_id)
			SELECT id, 'alice', 3 FROM sage.policy WHERE database_id = 41`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("simulate the old install (%s): %v", sql, err)
		}
	}
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var ids, distinct int
	var identity, legacy int64
	if err := pool.QueryRow(ctx, `SELECT count(id), count(DISTINCT id),
		(SELECT legacy_max_id FROM sage.audit_chain_meta WHERE chain = 'policy_approvals'),
		(SELECT count(*) FROM pg_attribute WHERE attrelid = 'sage.policy_approvals'::regclass
		 AND attname = 'id' AND attidentity = 'a')
		FROM sage.policy_approvals`).Scan(&ids, &distinct, &legacy, &identity); err != nil {
		t.Fatalf("read the upgraded table: %v", err)
	}
	if ids != 2 || distinct != 2 || legacy != 2 || identity != 1 {
		t.Fatalf("old approvals: %d ids (%d distinct), legacy boundary %d, identity %d",
			ids, distinct, legacy, identity)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.policy_approvals (policy_id, approver,
		approver_user_id) SELECT id, 'bob', 4 FROM sage.policy WHERE version = 2`); err != nil {
		t.Fatalf("approve after the upgrade: %v", err)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PolicyApprovals, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 1 || rep.LegacyRows != 2 {
		t.Fatalf("approvals chain after the upgrade: %+v (%v)", rep, err)
	}
	var unchained int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision WHERE chain_hash IS NULL
		AND id > (SELECT legacy_max_id FROM sage.audit_chain_meta WHERE chain = 'decision')`).
		Scan(&unchained); err != nil || unchained != 0 {
		t.Fatalf("unchained decisions after bootstrap: %d (%v)", unchained, err)
	}
}
