package testdb

import (
	"context"
	"testing"
	"time"
)

// SupabaseRoles are the cluster-wide roles Supabase installs; agent posture
// treats them as exposed once both exist.
var SupabaseRoles = []string{"anon", "authenticated"}

// HoldSupabaseRoles serializes every test that creates the Supabase roles,
// across packages running in parallel on one server, and drops those roles
// (with what they own or hold in dsn's database) when the test ends, before
// the lock is released. A test that creates anon or authenticated must call
// it first, so the roles never outlive their test or race another one.
func HoldSupabaseRoles(t testing.TB, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	release, err := LockCluster(ctx, dsn, "supabase_roles")
	if err != nil {
		t.Fatalf("supabase roles lock: %v", err)
	}
	t.Cleanup(func() {
		defer release()
		if err := DropSupabaseRoles(context.Background(), dsn); err != nil {
			t.Errorf("drop the Supabase roles: %v", err)
		}
	})
}
