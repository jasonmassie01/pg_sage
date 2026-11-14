package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PinnedPath is SHOW search_path on a session PinnedPool opened.
const PinnedPath = "pg_catalog, pg_temp"

// AppPathDatabase creates a disposable database whose application path is
// set with ALTER DATABASE ... SET search_path = app, public, holding
// app.orders (id, app_only), app."Order Items" (id, qty) and a decoy
// sage.orders (id, sage_only) in a schema named like pg_sage's: a plan of
// "SELECT app_only FROM orders" succeeds only on the application's path.
// It returns an unpinned admin pool and a one-session pinned pool
// (security-01c-n), so a test can read that session's path afterwards.
func AppPathDatabase(t testing.TB, label string) (admin, pinned *pgxpool.Pool) {
	t.Helper()
	dsn := CreateDatabase(t, label)
	admin = adminPool(t, dsn)
	ctx := context.Background()
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("read the database name: %v", err)
	}
	execAll(t, admin, []string{
		"CREATE SCHEMA app",
		"CREATE TABLE app.orders (id int PRIMARY KEY, app_only int)",
		`CREATE TABLE app."Order Items" (id int PRIMARY KEY, qty int)`,
		"CREATE SCHEMA sage",
		"CREATE TABLE sage.orders (id int PRIMARY KEY, sage_only int)",
		"ALTER DATABASE " + pgx.Identifier{db}.Sanitize() + " SET search_path = app, public",
	})
	return admin, pinnedPool(t, dsn, 1)
}

// DefaultPathDatabase creates a disposable database with no
// pg_db_role_setting row, holding public.orders (id, public_only) and a
// decoy orders (id, user_only) in a schema named after the login role: on
// the server's default path without "$user", "SELECT public_only FROM
// orders" plans and "SELECT user_only FROM orders" does not. It returns a
// one-session pinned pool.
func DefaultPathDatabase(t testing.TB, label string) *pgxpool.Pool {
	t.Helper()
	dsn := CreateDatabase(t, label)
	admin := adminPool(t, dsn)
	var role string
	if err := admin.QueryRow(context.Background(), "SELECT current_user").
		Scan(&role); err != nil {
		t.Fatalf("read the login role: %v", err)
	}
	user := pgx.Identifier{role}.Sanitize()
	execAll(t, admin, []string{
		"CREATE TABLE public.orders (id int PRIMARY KEY, public_only int)",
		"CREATE SCHEMA IF NOT EXISTS " + user,
		"CREATE TABLE " + user + ".orders (id int PRIMARY KEY, user_only int)",
	})
	return pinnedPool(t, dsn, 1)
}

// SessionPath is SHOW search_path on one of pool's sessions (the only one
// of a one-session pool).
func SessionPath(t testing.TB, pool *pgxpool.Pool) string {
	t.Helper()
	var path string
	if err := pool.QueryRow(context.Background(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatalf("SHOW search_path: %v", err)
	}
	return path
}

func adminPool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	return admin
}

func execAll(t testing.TB, pool *pgxpool.Pool, stmts []string) {
	t.Helper()
	for _, sql := range stmts {
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}
