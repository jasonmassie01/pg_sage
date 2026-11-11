package auditchain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// audit-11: a policy approval is the human authority behind a widening
// policy version. sage.policy_approvals gains an id and its own chain;
// review_status (the post-hoc review) is its only state.

// insertApproval stores a policy version and one approval of it, and
// returns the approval's id.
func insertApproval(t *testing.T, pool *pgxpool.Pool, version int, approver string) int64 {
	t.Helper()
	ctx := context.Background()
	var policyID, id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.policy (database_id, version, doc,
		status, proposed_by) VALUES (41, $1, '{}', 'proposed', 'agent') RETURNING id`,
		version).Scan(&policyID); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.policy_approvals (policy_id, approver,
		approver_user_id, reason, single_operator, review_status) VALUES ($1, $2, 3,
		'reviewed the diff', true, 'pending') RETURNING id`, policyID,
		approver).Scan(&id); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
	return id
}

// An approval is chained on insert; its review is one state link; deleting
// it behind the chain is reported, deleting it through the triggers is a
// recorded delete and verifies.
func TestPolicyApprovalChain(t *testing.T) {
	pool := freshDB(t, "chain_approvals")
	hidden := insertApproval(t, pool, 1, "alice")
	reviewed := insertApproval(t, pool, 2, "bob")
	removed := insertApproval(t, pool, 3, "carol")
	if hidden <= 0 || reviewed == hidden || removed == reviewed {
		t.Fatalf("approval ids %d %d %d are not distinct identities", hidden, reviewed,
			removed)
	}
	exec(t, pool, `UPDATE sage.policy_approvals SET review_status = 'reviewed'
		WHERE id = $1`, reviewed)
	exec(t, pool, "DELETE FROM sage.policy_approvals WHERE id = $1", removed)
	rep := verify(t, pool, auditchain.PolicyApprovals, auditchain.Window{})
	// 3 inserts, 1 review, 1 delete.
	if !rep.OK() || rep.Links != 5 {
		t.Fatalf("approvals chain before tampering: %+v", rep)
	}
	bypass(t, pool, "DELETE FROM sage.policy_approvals WHERE id = $1", hidden)
	bypass(t, pool, `UPDATE sage.policy_approvals SET approver_user_id = 9
		WHERE id = $1`, reviewed)
	rep = verify(t, pool, auditchain.PolicyApprovals, auditchain.Window{})
	kinds := problemKinds(rep)
	if rep.OK() || !containsKind(kinds[hidden], auditchain.KindRowMissing) ||
		!containsKind(kinds[reviewed], auditchain.KindRowEdited) ||
		len(kinds[removed]) != 0 {
		t.Fatalf("tampered approvals: problems %+v", rep.Problems)
	}
}

// A retention purge of withheld decisions is a run of recorded deletes by
// pg_sage's role under its application name: the chain verifies clean.
func TestRetentionPurgeOfWithheldDecisionsVerifiesClean(t *testing.T) {
	pool := freshDB(t, "chain_decision_retention")
	ctx := context.Background()
	exec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id, created_at, last_seen_at)
		SELECT 'index', 'i', 'parked', 'moderate', 'outside window', 'ev-old-' || g,
		       now() - interval '90 days', now() - interval '90 days'
		  FROM generate_series(1, 25) g`)
	kept := insertDecision(t, pool, "parked", "moderate", "")
	sagePool := appPool(t, pool, "pg_sage")
	cfg := config.DefaultConfig()
	cfg.Retention.DecisionsDays = 30
	retention.New(sagePool, cfg, func(string, string, ...any) {}).RunOnce(ctx)
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.decision").Scan(&left); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if left != 1 {
		t.Fatalf("%d decisions left after the purge, want only %d", left, kept)
	}
	var deletes, foreign int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE db_user <>
		current_user OR app <> 'pg_sage') FROM sage.audit_chain_link
		WHERE chain = 'decision' AND op = 'D'`).Scan(&deletes, &foreign); err != nil {
		t.Fatalf("count delete links: %v", err)
	}
	if deletes != 25 || foreign != 0 {
		t.Fatalf("delete links = %d (%d not pg_sage's), want 25 of pg_sage's", deletes,
			foreign)
	}
	rep := verify(t, pool, auditchain.DecisionLedger, auditchain.Window{})
	if !rep.OK() || rep.Links != 51 || rep.RowsChecked != 1 {
		t.Fatalf("after the purge: %+v", rep)
	}
}

// appPool opens a second pool on pool's database under application name.
func appPool(t *testing.T, pool *pgxpool.Pool, app string) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", app, err)
	}
	t.Cleanup(p.Close)
	return p
}

// On a database without the agent-gate and ledger tables the install
// skips their chains, and verifying them reports that the chain is not
// installed rather than a clean or a broken trail.
func TestApprovalAndDecisionChainsSkipAbsentTables(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "chain_approvals_absent")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	exec(t, pool, "CREATE SCHEMA sage")
	for i := 0; i < 2; i++ { // the install is idempotent
		if _, err := pool.Exec(ctx, auditchain.MigrationSQL()); err != nil {
			t.Fatalf("install %d without the tables: %v", i+1, err)
		}
	}
	var chains []string
	if err := pool.QueryRow(ctx, `SELECT coalesce(array_agg(chain), '{}')
		FROM sage.audit_chain_meta`).Scan(&chains); err != nil {
		t.Fatalf("installed chains: %v", err)
	}
	if len(chains) != 0 {
		t.Fatalf("chains installed without their tables: %v", chains)
	}
	for _, spec := range []auditchain.Spec{auditchain.PolicyApprovals,
		auditchain.DecisionLedger} {
		if !spec.Optional {
			t.Fatalf("%s must be optional: its table is another workstream's", spec.Chain)
		}
		_, err := auditchain.Verify(ctx, pool, spec, auditchain.Window{})
		if !errors.Is(err, auditchain.ErrNotInstalled) ||
			!strings.Contains(err.Error(), spec.Chain) {
			t.Fatalf("verify %s without its table: %v, want ErrNotInstalled", spec.Chain, err)
		}
	}
}
