package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Agent provenance on the approval queue (AGENTDB-SPEC §6.2.2 D9, §7): an
// item an agent's request queued carries its principal, the approved run
// reads it back, and the per-principal pending count (D9
// max_pending_per_principal) uses the (principal_id, status) index.

const queuePrincipal = "agp_qqqqqqqqqqqqqqqqqqqq"

func cleanPrincipalQueue(t *testing.T, ctx context.Context, principals ...string) {
	t.Helper()
	clean := func() {
		_, _ = testPool.Exec(ctx, "DELETE FROM sage.action_queue WHERE principal_id = ANY($1)",
			principals)
	}
	clean()
	t.Cleanup(clean)
}

func TestProposeWithMetadata_RecordsThePrincipal(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanPrincipalQueue(t, ctx, queuePrincipal)
	s := NewActionStore(pool)
	id, err := s.ProposeWithMetadata(ctx, nil, 0, "SELECT 1", "", "safe",
		ActionProposalMetadata{PrincipalID: queuePrincipal, ProposedVia: "agent",
			ProposedBy: queuePrincipal})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	got, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PrincipalID != queuePrincipal || got.ProposedVia != "agent" {
		t.Fatalf("principal %q via %q", got.PrincipalID, got.ProposedVia)
	}
	own, err := s.ProposeWithMetadata(ctx, nil, 0, "SELECT 2", "", "safe",
		ActionProposalMetadata{})
	if err != nil {
		t.Fatalf("propose own: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE id = $1", own) }()
	got, err = s.GetByID(ctx, own)
	if err != nil || got.PrincipalID != "" {
		t.Fatalf("pg_sage's own item has principal %q (%v)", got.PrincipalID, err)
	}
}

func TestCountPendingForPrincipal(t *testing.T) {
	pool, ctx := requireDB(t)
	other := "agp_rrrrrrrrrrrrrrrrrrrr"
	cleanPrincipalQueue(t, ctx, queuePrincipal, other)
	s := NewActionStore(pool)
	n, err := s.CountPendingForPrincipal(ctx, queuePrincipal)
	if err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	past := time.Now().Add(-time.Minute)
	for i, meta := range []ActionProposalMetadata{
		{PrincipalID: queuePrincipal}, {PrincipalID: queuePrincipal},
		{PrincipalID: queuePrincipal, ExpiresAt: &past}, {PrincipalID: other},
	} {
		if _, err := s.ProposeWithMetadata(ctx, nil, 0, fmt.Sprintf("SELECT %d", i), "",
			"safe", meta); err != nil {
			t.Fatalf("propose %d: %v", i, err)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE sage.action_queue SET status = 'approved'
		WHERE id = (SELECT min(id) FROM sage.action_queue WHERE principal_id = $1)`,
		queuePrincipal)
	if err != nil {
		t.Fatal(err)
	}
	n, err = s.CountPendingForPrincipal(ctx, queuePrincipal)
	if err != nil || n != 1 {
		t.Fatalf("pending, unexpired, own: got %d (%v), want 1", n, err)
	}
	if _, err := s.CountPendingForPrincipal(ctx, ""); err == nil {
		t.Fatal("an empty principal must be refused")
	}
}

// The count stays inside the perf gate: with many other items queued it
// reads the (principal_id, status) index, never a sequential scan.
func TestCountPendingForPrincipal_UsesTheIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	cleanPrincipalQueue(t, ctx, queuePrincipal)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
		principal_id, status) SELECT 'SELECT 1', 'safe',
		'agp_' || lpad(translate((g % 500)::text, '0123456789', 'abcdefghij'), 20, 'a'),
		CASE WHEN g % 3 = 0 THEN 'pending' ELSE 'approved' END
		FROM generate_series(1, 20000) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "ANALYZE sage.action_queue"); err != nil {
		t.Fatal(err)
	}
	plan, err := testdb.Explain(ctx, tx, "", countPendingForPrincipalSQL, queuePrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeqScans("action_queue") != 0 {
		t.Fatalf("sequential scan of action_queue:\n%s", plan)
	}
	used := false
	plan.Walk(func(n testdb.PlanNode) { used = used || n.Index == "action_queue_principal_status" })
	if !used {
		t.Fatalf("plan does not use action_queue_principal_status:\n%s", plan)
	}
}
