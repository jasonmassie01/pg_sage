package schema

import (
	"strings"
	"testing"
)

// earned-06: the ledger history admits evidence_expiring after bootstrap
// (run twice), a database whose check predates it gains it with every
// earlier type kept, and a re-run changes nothing.
func TestSREMigrationEvidenceExpiring_UpgradesIdempotently(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	if def := autonomyTypeCheck(t, pool); !strings.Contains(def, "'evidence_expiring'") {
		t.Fatalf("bootstrapped check lacks evidence_expiring: %s", def)
	}
	// Rolled back: other packages share the database.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.sre_autonomy_events
		DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, ddlSREAutonomyRootAuthority); err != nil {
		t.Fatalf("restore the root-authority check: %v", err)
	}
	if def := autonomyTypeCheck(t, tx); strings.Contains(def, "evidence_expiring") {
		t.Fatalf("the older check already admits evidence_expiring: %s", def)
	}
	if _, err := tx.Exec(ctx, ddlSREAutonomyEvidenceExpiring); err != nil {
		t.Fatalf("migration: %v", err)
	}
	first := autonomyTypeCheck(t, tx)
	for _, typ := range []string{"evidence_expiring", "root_authority_revoked",
		"grandfathered", "database_scoped", "carried_over", "capped", "cap_cleared"} {
		if !strings.Contains(first, "'"+typ+"'") {
			t.Fatalf("after the migration the check lacks %q: %s", typ, first)
		}
	}
	if _, err := tx.Exec(ctx, ddlSREAutonomyEvidenceExpiring); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again := autonomyTypeCheck(t, tx); again != first {
		t.Fatalf("a re-run changed the check: %s / %s", first, again)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, actor, reason)
		VALUES ('t', 'f', 'c', 'evidence_expiry_typo', 'pg_sage', 'x')`); err == nil {
		t.Fatal("an unknown event type was accepted")
	}
}
