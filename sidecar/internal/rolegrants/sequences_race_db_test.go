package rolegrants

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Concurrent access: a sequence dropped while the probe runs. The probe's
// pg_class scan still sees it (the statement's snapshot) while
// has_sequence_privilege, which looks it up in the catalog cache, returns
// NULL for it. A schema whose only sequence went that way must read as
// nothing to grant, never fail the whole check (and so the Grant more
// guide and the startup check) with a NULL scanned into a bool.
func TestCheckSequenceSelectSequenceDroppedMidProbe(t *testing.T) {
	db, ctx := newGrantsDB(t)
	noFixtureSequences(t, ctx, db)
	if _, err := db.admin.Exec(ctx, "CREATE SCHEMA racy; CREATE SEQUENCE racy.s"); err != nil {
		t.Fatalf("create racy.s: %v", err)
	}
	tx, err := db.admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil { // takes the snapshot
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := db.admin.Exec(ctx, "DROP SEQUENCE racy.s"); err != nil {
		t.Fatalf("drop racy.s: %v", err)
	}
	got, err := readSequenceSelect(ctx, tx)
	if err != nil {
		t.Fatalf("probe with a sequence dropped mid-probe: %v", err)
	}
	if got.Lacking() {
		t.Fatalf("got %+v: a dropped sequence needs no grant", got)
	}
	if len(got.Schemas) != 1 || got.Schemas[0] != "racy" {
		t.Fatalf("schemas = %q, want the snapshot's racy only", got.Schemas)
	}
}
