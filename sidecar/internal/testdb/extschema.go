package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExtSchema is the schema ExtSchemaDatabase installs the extensions in.
const ExtSchema = "ext"

// ExtSchemaDatabase creates a disposable database with pg_stat_statements,
// pgstattuple and hypopg installed in schema ext, and returns an unpinned
// admin pool and a pool whose sessions run under search_path = pg_catalog,
// pg_temp (security-01c): a reader that names an extension object without
// its schema fails on pinned. It skips when no live server is configured.
func ExtSchemaDatabase(t testing.TB, label string) (admin, pinned *pgxpool.Pool) {
	t.Helper()
	dsn := CreateDatabase(t, label)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	for _, sql := range []string{
		"DROP EXTENSION IF EXISTS pg_stat_statements",
		"DROP EXTENSION IF EXISTS pgstattuple",
		"DROP EXTENSION IF EXISTS hypopg",
		"CREATE SCHEMA " + ExtSchema,
		"CREATE EXTENSION pg_stat_statements SCHEMA " + ExtSchema,
		"CREATE EXTENSION pgstattuple SCHEMA " + ExtSchema,
		"CREATE EXTENSION hypopg SCHEMA " + ExtSchema,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return admin, PinnedPool(t, dsn)
}

// PinnedPool opens a pool on dsn whose sessions run under search_path =
// pg_catalog, pg_temp, as pg_sage's observation sessions will.
func PinnedPool(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Exec(ctx, "SET search_path = pg_catalog, pg_temp").Close()
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pinned pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SeedStatement runs sql on pool calls times and returns its queryid in
// ext.pg_stat_statements, read through admin (unpinned).
func SeedStatement(t testing.TB, admin, pool *pgxpool.Pool, sql string, calls int) int64 {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < calls; i++ {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var id int64
	err := admin.QueryRow(ctx, `SELECT queryid FROM `+ExtSchema+`.pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query = $1 ORDER BY calls DESC LIMIT 1`, sql).Scan(&id)
	if err != nil {
		t.Fatalf("queryid of %q: %v", sql, err)
	}
	return id
}
