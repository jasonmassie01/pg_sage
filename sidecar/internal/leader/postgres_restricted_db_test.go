package leader

import (
	"context"
	"testing"
	"time"
)

// PostgreSQL checks EXECUTE on a function when the plan starts, not when
// the expression runs. After five custom plans a prepared statement may
// switch to a generic plan, where the "read it" flag is a parameter, so
// a statement that only skips pg_control_system() at run time fails for a
// role without EXECUTE from the sixth call on. Renewals must keep working
// for the whole run on such a server (fleet-03 review).
func TestPostgresStore_RestrictedRoleKeepsRenewingPastThePlanCache(t *testing.T) {
	ctx := context.Background()
	admin := dbPool(t)
	db := freshDatabase(t, admin, "sage_leader_restricted_renew")
	role := "sage_leader_restricted_renew"
	_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	stmts := []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD '" + role + "'",
		"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC",
		"GRANT USAGE ON SCHEMA sage TO " + role,
		"GRANT SELECT, INSERT, UPDATE ON sage.fleet_leader_lease TO " + role,
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role) })
	pool := poolOn(t, "sage_leader_restricted_renew", role)
	st := NewPostgresStore(pool)
	for i := 0; i < 20; i++ {
		l, held, err := st.Acquire(ctx, "restricted", "A", time.Minute)
		if err != nil || !held || l.Control.Datname != "sage_leader_restricted_renew" ||
			l.Control.SystemIdentifier != "" {
			t.Fatalf("renewal %d = %+v held=%v err=%v, want held with datname only",
				i+1, l, held, err)
		}
		cur, ok, err := st.Current(ctx, "restricted")
		if err != nil || !ok || cur.Holder != "A" || cur.Control.SystemIdentifier != "" {
			t.Fatalf("read %d = %+v ok=%v err=%v, want A's lease", i+1, cur, ok, err)
		}
	}
	// Another holder is refused, and reads the live lease without error.
	other := NewPostgresStore(poolOn(t, "sage_leader_restricted_renew", role))
	for i := 0; i < 10; i++ {
		l, held, err := other.Acquire(ctx, "restricted", "B", time.Minute)
		if err != nil || held || l.Holder != "A" {
			t.Fatalf("contender %d = %+v held=%v err=%v, want A's live lease", i+1, l,
				held, err)
		}
	}
}
