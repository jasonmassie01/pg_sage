//go:build integration

package apppath

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// pinnedPool opens a one-connection pool on a fresh database whose
// sessions run under search_path = pg_catalog, pg_temp, as pg_sage's
// observation sessions do; admin is an unpinned pool on the same database.
func pinnedPool(t *testing.T) (pool, admin *pgxpool.Pool, db string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "apppath")
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Exec(ctx, "SET search_path = pg_catalog, pg_temp").Close()
	}
	pool, err = pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pinned pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("database name: %v", err)
	}
	return pool, admin, db
}

func adminExec(t *testing.T, admin *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// pinnedSchemas pins the application path in a fresh transaction of pool
// and returns the schemas it resolves through, then the session's
// search_path after the transaction ends.
func pinnedSchemas(t *testing.T, pool *pgxpool.Pool) ([]string, string) {
	t.Helper()
	pool.Reset() // a new session picks up ALTER DATABASE / ROLE settings
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Pin(t.Context(), tx); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("Pin: %v", err)
	}
	var schemas []string
	err = tx.QueryRow(t.Context(), "SELECT pg_catalog.current_schemas(false)").Scan(&schemas)
	if err != nil {
		t.Fatalf("current_schemas: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var after string
	if err := pool.QueryRow(t.Context(), "SHOW search_path").Scan(&after); err != nil {
		t.Fatalf("show search_path: %v", err)
	}
	return schemas, after
}

// With no setting, the server's own value applies ("$user", public),
// without "$user": pg_sage's role is not the application's.
func TestPinUsesTheServerValueWithoutSettings(t *testing.T) {
	pool, admin, _ := pinnedPool(t)
	adminExec(t, admin, "CREATE SCHEMA postgres") // what "$user" would find
	schemas, after := pinnedSchemas(t, pool)
	if want := []string{"public"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want %q", schemas, want)
	}
	if after != "pg_catalog, pg_temp" {
		t.Fatalf("search_path after the transaction = %q, want the session pin back", after)
	}
}

// The database's setting is the application's path, and a setting on
// pg_sage's own role is never used: it is not the application's.
func TestPinUsesTheDatabaseSettingNotPgSagesOwn(t *testing.T) {
	pool, admin, db := pinnedPool(t)
	ident := pgx.Identifier{db}.Sanitize()
	adminExec(t, admin, "CREATE SCHEMA app", "CREATE SCHEMA sage_own",
		"ALTER DATABASE "+ident+" SET search_path = app, public",
		"ALTER ROLE CURRENT_USER IN DATABASE "+ident+" SET search_path = sage_own")
	schemas, _ := pinnedSchemas(t, pool)
	if want := []string{"app", "public"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want the database setting %q", schemas, want)
	}
}

// State transition: with only pg_sage's own role setting, the server
// value is unknown (RESET would restore pg_sage's), so PostgreSQL's
// default applies; once the database gets a setting, that one does.
func TestPinIgnoresPgSagesOwnSettingAsTheServerValue(t *testing.T) {
	pool, admin, db := pinnedPool(t)
	ident := pgx.Identifier{db}.Sanitize()
	adminExec(t, admin, "CREATE SCHEMA app", "CREATE SCHEMA sage_own",
		"ALTER ROLE CURRENT_USER IN DATABASE "+ident+" SET search_path = sage_own")
	schemas, _ := pinnedSchemas(t, pool)
	if want := []string{"public"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want PostgreSQL's default %q", schemas, want)
	}
	adminExec(t, admin, "ALTER DATABASE "+ident+" SET search_path = app")
	schemas, _ = pinnedSchemas(t, pool)
	if want := []string{"app"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want the new database setting %q", schemas, want)
	}
}

// A quoted mixed-case schema in the setting resolves exactly.
func TestPinKeepsQuotedSchemaNames(t *testing.T) {
	pool, admin, db := pinnedPool(t)
	ident := pgx.Identifier{db}.Sanitize()
	adminExec(t, admin, `CREATE SCHEMA "App Data"`,
		"ALTER DATABASE "+ident+` SET search_path = "App Data", public`)
	schemas, _ := pinnedSchemas(t, pool)
	if want := []string{"App Data", "public"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want %q", schemas, want)
	}
}

// pg_sage on a managed cloud is no superuser: the path read must work for
// a plain login role, which can read pg_db_role_setting, pg_roles and
// pg_settings but holds no privilege beyond USAGE on the schema.
func TestPinWorksForAPlainLoginRole(t *testing.T) {
	_, admin, db := pinnedPool(t)
	ident := pgx.Identifier{db}.Sanitize()
	adminExec(t, admin, "CREATE SCHEMA app",
		"ALTER DATABASE "+ident+" SET search_path = app, public",
		"CREATE ROLE apppath_plain LOGIN PASSWORD 'apppath_plain'",
		"GRANT USAGE ON SCHEMA app TO apppath_plain")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP OWNED BY apppath_plain")
		_, _ = admin.Exec(ctx, "DROP ROLE apppath_plain")
	})
	cfg, err := pgxpool.ParseConfig(admin.Config().ConnString())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "apppath_plain", "apppath_plain"
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Exec(ctx, "SET search_path = pg_catalog, pg_temp").Close()
	}
	plain, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("plain role pool: %v", err)
	}
	t.Cleanup(plain.Close)
	var super bool
	if err := plain.QueryRow(t.Context(),
		"SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user",
	).Scan(&super); err != nil || super {
		t.Fatalf("plain role: superuser=%v err=%v, want a plain role", super, err)
	}
	schemas, after := pinnedSchemas(t, plain)
	if want := []string{"app", "public"}; !reflect.DeepEqual(schemas, want) {
		t.Fatalf("schemas = %q, want the database setting %q", schemas, want)
	}
	if after != "pg_catalog, pg_temp" {
		t.Fatalf("search_path after the transaction = %q, want the session pin back", after)
	}
}
