//go:build integration

package apppath

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: a name an application's SQL writes resolves on the
// application's path even from a session pinned to pg_catalog, pg_temp.
// No concurrent-access test: each call has its own transaction and the
// path is transaction-local.

func resolveOn(t *testing.T, db catalogread.Beginner, name string) string {
	t.Helper()
	got, err := Resolve(t.Context(), db, catalogread.Default(), name)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	return got
}

// Happy path and boundary: unqualified, quoted and qualified names
// resolve on app, public; the sage schema never shadows app's table.
func TestResolve_NamesTheApplicationsRelationUnderThePin(t *testing.T) {
	admin, pinned := testdb.AppPathDatabase(t, "apppath_resolve")
	cases := map[string]string{
		"orders":                 "app.orders",
		"ORDERS":                 "app.orders",
		`"Order Items"`:          `app."Order Items"`,
		`app."Order Items"`:      `app."Order Items"`,
		"sage.orders":            "sage.orders",
		"pg_catalog.pg_class":    "pg_catalog.pg_class",
		"app.orders_pkey":        "app.orders_pkey",
		"orders_pkey":            "app.orders_pkey",
		"missing":                "",
		"sage.missing":           "",
		`"orders"."Order Items"`: "",
		"public.orders":          "",
	}
	for name, want := range cases {
		if got := resolveOn(t, pinned, name); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}
	if got := resolveOn(t, admin, "orders"); got != "app.orders" {
		t.Errorf("unpinned Resolve(orders) = %q, want app.orders", got)
	}
	// State transition: the session is back on its pin afterwards.
	if got := testdb.SessionPath(t, pinned); got != testdb.PinnedPath {
		t.Fatalf("search_path after Resolve = %q, want %q", got, testdb.PinnedPath)
	}
}

// Nil/empty: with no pg_db_role_setting row the server default applies
// without "$user": the role-named schema's table is not found first.
func TestResolve_ServerDefaultPathLeavesOutUser(t *testing.T) {
	pinned := testdb.DefaultPathDatabase(t, "apppath_resolve_default")
	if got := resolveOn(t, pinned, "orders"); got != "public.orders" {
		t.Fatalf("Resolve(orders) = %q, want public.orders", got)
	}
}

// Boundary: an application path that puts pg_catalog last never runs a
// function the application planted under a catalog name.
func TestResolve_PlantedFunctionOnTheApplicationPathIsNeverCalled(t *testing.T) {
	admin, pinned := testdb.AppPathDatabase(t, "apppath_resolve_planted")
	var db string
	if err := admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("database name: %v", err)
	}
	adminExec(t, admin,
		`CREATE FUNCTION app.quote_ident(text) RETURNS text LANGUAGE sql
		 AS $$ SELECT 'planted' $$`,
		`CREATE FUNCTION app.to_regclass(text) RETURNS regclass LANGUAGE sql
		 AS $$ SELECT 'pg_catalog.pg_class'::pg_catalog.regclass $$`,
		"ALTER DATABASE "+pgx.Identifier{db}.Sanitize()+
			" SET search_path = app, public, pg_catalog")
	pinned.Reset()
	if got := resolveOn(t, pinned, "orders"); got != "app.orders" {
		t.Fatalf("Resolve(orders) = %q, want app.orders (no planted function)", got)
	}
}

// Invalid input and error propagation: a malformed name fails with the
// PostgreSQL error naming it, and an empty name is refused before SQL.
func TestResolve_MalformedNameIsAnError(t *testing.T) {
	_, pinned := testdb.AppPathDatabase(t, "apppath_resolve_bad")
	got, err := Resolve(t.Context(), pinned, catalogread.Default(), "a.b.c.d")
	if err == nil || got != "" {
		t.Fatalf("Resolve(a.b.c.d) = %q, %v; want an error", got, err)
	}
	if !strings.Contains(err.Error(), "a.b.c.d") {
		t.Fatalf("error %q does not name the relation", err)
	}
}
