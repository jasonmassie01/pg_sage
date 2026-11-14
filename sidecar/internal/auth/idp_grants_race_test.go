package auth

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-08d: a manual grant an admin writes while a login's sync is in
// flight still wins. The sync read the grants before the admin's row was
// committed, so only the upsert's WHERE source = 'idp' keeps it manual.
func TestIdPGrantSync_ManualRowWrittenMidSyncIsNotOverwritten(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "idp-midsync-admin@example.com", RoleAdmin)
	u := linkTestUser(t, pool, "idp-midsync@example.com", RoleOperator)
	wide := idpWidePool(t, pool)
	tx := uncommittedManualGrant(t, wide, u, admin)
	type outcome struct {
		res IdPGrantResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewDatabaseGrantStore(wide).SyncIdPGrants(ctx, IdPGrantSync{UserID: u,
			Grants: map[string]string{"billing": RoleOperator}})
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		t.Fatalf("sync did not wait for the uncommitted manual row: %+v", o)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sync never finished after the manual grant committed")
	}
	if o.err != nil || len(o.res.Set) != 0 || len(o.res.Shadowed) != 1 ||
		o.res.Shadowed[0] != "billing" {
		t.Fatalf("sync = (%+v, %v), want billing shadowed and nothing set", o.res, o.err)
	}
	if rows := idpGrantRows(t, pool, u); len(rows) != 1 ||
		rows["billing"] != (idpRow{RoleViewer, GrantSourceManual}) {
		t.Fatalf("rows = %+v, want the admin's manual viewer grant", rows)
	}
	if n := len(idpAuditDetails(t, pool, AuditDatabaseGrantSet, u)); n != 0 {
		t.Fatalf("%d database_grant_set rows for a grant that was not written", n)
	}
}

// uncommittedManualGrant opens a transaction holding an uncommitted manual
// viewer grant on billing for u; the test commits it, cleanup rolls back.
func uncommittedManualGrant(t *testing.T, pool *pgxpool.Pool, u, admin int) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, granted_by, source)
		VALUES ($1, 'billing', 'viewer', $2, 'manual')`, u, admin); err != nil {
		t.Fatalf("uncommitted manual grant: %v", err)
	}
	return tx
}
