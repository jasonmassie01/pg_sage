package ledger

import (
	"testing"
)

// Regression test for G4-B27: the self-audit must not flag rows whose
// verification is still pending, nor re-report historic rows forever.
func TestSelfAuditIgnoresPendingAndHistoricRows(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID := ledgerDatabaseID()
	monitoring := insertAuditAction(t, ctx, pool, databaseID, "monitoring", nil)
	historic := insertAuditAction(t, ctx, pool, databaseID, "success", nil)
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log
		SET executed_at = now() - interval '3 days' WHERE id=$1`, historic); err != nil {
		t.Fatalf("age action: %v", err)
	}
	current := insertAuditAction(t, ctx, pool, databaseID, "success", nil)

	violations, err := repo.FindAuditViolations(ctx)

	if err != nil {
		t.Fatalf("FindAuditViolations: %v", err)
	}
	ours := violationsForActions(violations, map[int64]bool{
		monitoring: true, historic: true, current: true,
	})
	if len(ours) != 1 || ours[0].ActionID != current {
		t.Fatalf("violations = %#v, want only current action %d", ours, current)
	}
}
