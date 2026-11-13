package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access test: the meta lifecycle tests own the package
// database and run sequentially by design (see the note above
// metaLifecycleFixture).

// config-14: in a sliced or shuffled run (CI's nightly -shuffle=on job) a
// test that signs in a different user can run before the first
// metaLifecycleFixture caller. The fixture must still hand back the admin
// it promises instead of failing with "no rows in result set".
func TestMetaLifecycleFixtureWhenAnotherUserExistsFirst(t *testing.T) {
	ctx := context.Background()
	pool, err := connectMetaDB(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	other := "config14-other@pg-sage.local"
	if _, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE email = $1",
		other); err != nil {
		t.Fatalf("clear earlier other user: %v", err)
	}
	if _, err := auth.CreateUser(ctx, pool, other, "other-user-password",
		auth.RoleViewer); err != nil {
		t.Fatalf("create the other user first: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.users WHERE email = $1", other)
	})
	parkFixtureAdmin(t, pool)

	_, _, userID := metaLifecycleFixture(t)

	var email, role string
	var mustChange bool
	err = pool.QueryRow(ctx, "SELECT email, role, must_change_password "+
		"FROM sage.users WHERE id = $1", userID).Scan(&email, &role, &mustChange)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fixture returned user id %d, which does not exist", userID)
	}
	if err != nil {
		t.Fatalf("read fixture user: %v", err)
	}
	if email != adminEmail || role != auth.RoleAdmin || mustChange {
		t.Fatalf("fixture user = %q role %q must_change=%v, want %q admin false",
			email, role, mustChange, adminEmail)
	}
}

// parkFixtureAdmin moves an admin left by an earlier fixture caller aside,
// so the fixture must create its own while another user exists. Without
// it the test above passes on the broken fixture in any whole-package run.
// Cleanup puts the earlier admin back in place of the one the fixture made.
func parkFixtureAdmin(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	parked := "config14-parked-admin@pg-sage.local"
	tag, err := pool.Exec(ctx, "UPDATE sage.users SET email = $1 WHERE email = $2",
		parked, adminEmail)
	if err != nil {
		t.Fatalf("park the earlier fixture admin: %v", err)
	}
	if tag.RowsAffected() == 0 {
		return
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE email = $1",
			adminEmail); err != nil {
			t.Errorf("remove the admin the fixture created: %v", err)
			return
		}
		if _, err := pool.Exec(ctx, "UPDATE sage.users SET email = $1 WHERE email = $2",
			adminEmail, parked); err != nil {
			t.Errorf("restore the earlier fixture admin: %v", err)
		}
	})
}

// Invalid input does not apply: the fixture takes no arguments. The id it
// returns must be the admin row's id, whichever path created that row.
func TestMetaLifecycleFixtureReturnsTheAdminRow(t *testing.T) {
	ctx := context.Background()
	_, _, userID := metaLifecycleFixture(t)
	pool, err := connectMetaDB(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var id int
	if err := pool.QueryRow(ctx, "SELECT id FROM sage.users WHERE email = $1",
		adminEmail).Scan(&id); err != nil {
		t.Fatalf("admin missing after fixture: %v", err)
	}
	if id != userID || userID <= 0 {
		t.Fatalf("fixture user id = %d, admin row id = %d", userID, id)
	}
}
