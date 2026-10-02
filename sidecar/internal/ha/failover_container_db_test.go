package ha

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A real failover while pg_sage is down: a monitor records a disposable
// standby, stops (as pg_sage would), the standby is promoted, and a new
// monitor with the same history must open the failover cooldown. Runs
// only when SAGE_TEST_HA_STANDBY_URL names a disposable streaming standby
// the test may promote (a superuser DSN); the history lives in the test
// database (SAGE_TEST_DATABASE_URL).
func TestContainer_FailoverWhileDownOpensTheCooldown(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_HA_STANDBY_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_HA_STANDBY_URL not set: no disposable standby to promote")
	}
	control, ctx := storePool(t)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	node, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the standby: %v", err)
	}
	defer node.Close()
	store := NewPostgresIdentityStore(control)
	key := uniqueKey(t)
	before := New(node, noopLog).WithIdentityStore(store, key)
	if notPrimary := before.Check(ctx); !notPrimary || before.Role() != RoleReplica {
		t.Skipf("SAGE_TEST_HA_STANDBY_URL is not a standby (role %s)", before.Role())
	}
	recorded := before.Identity()
	if recorded.TimelineID < 1 || recorded.SystemID == "" {
		t.Fatalf("standby identity = %+v", recorded)
	}
	var promoted bool
	if err := node.QueryRow(ctx, "SELECT pg_promote(true, 60)").Scan(&promoted); err != nil ||
		!promoted {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	start := time.Now()
	after := New(node, noopLog).WithIdentityStore(store, key)
	if notPrimary := after.Check(ctx); notPrimary {
		t.Fatal("the promoted node is not a primary")
	}
	if got := after.LastRoleChange(); got.Before(start) {
		t.Fatalf("a failover while down did not open the cooldown (change %v)", got)
	}
	now := after.Identity()
	if now.TimelineID <= recorded.TimelineID || now.SystemID != recorded.SystemID {
		t.Fatalf("identity %+v -> %+v, want the same cluster on a later timeline",
			recorded, now)
	}
}
