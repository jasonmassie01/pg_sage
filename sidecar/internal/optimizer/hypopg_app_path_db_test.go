package optimizer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c: the HypoPG what-if plans the workload on the
// application's search_path (the database's setting) even when pg_sage's
// session is pinned to pg_catalog, pg_temp and its own role path names a
// schema the application does not use.

// appPathHypoPool creates a database with HypoPG in its own schema, the
// workload table in schema app (the database's search_path) and a
// one-connection pool pinned as pg_sage's sessions are.
func appPathHypoPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "hypopg_app_path")
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	var available bool
	if err := admin.QueryRow(t.Context(), `SELECT EXISTS
		(SELECT 1 FROM pg_available_extensions WHERE name='hypopg')`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Skip("HypoPG server extension unavailable; application path not verified")
	}
	var db string
	if err := admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{db}.Sanitize()
	for _, sql := range []string{
		"CREATE SCHEMA hypo_ext", "CREATE EXTENSION hypopg WITH SCHEMA hypo_ext",
		"CREATE SCHEMA app", "CREATE SCHEMA sage_own",
		"CREATE TABLE app.items (id int, category int)",
		"INSERT INTO app.items SELECT i, i % 10000 FROM generate_series(1, 20000) i",
		"VACUUM ANALYZE app.items",
		"ALTER DATABASE " + ident + " SET search_path = app, public",
		"ALTER ROLE CURRENT_USER IN DATABASE " + ident + " SET search_path = sage_own",
	} {
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Exec(ctx, "SET search_path = pg_catalog, pg_temp").Close()
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestHypoPGPlansTheWorkloadOnTheApplicationPath(t *testing.T) {
	pool := appPathHypoPool(t)
	h := NewHypoPG(pool, noopLog2)
	queries := []QueryInfo{{QueryID: 1, Calls: 10, TotalTimeMs: 100,
		Text: "SELECT id FROM items WHERE category = 7"}}
	rec := Recommendation{DDL: "CREATE INDEX ON items (category)", AffectedQueryIDs: []int64{1}}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	res, err := h.Validate(ctx, rec, queries)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Measured != 1 || res.Failed != 0 {
		t.Fatalf("result %+v: want the unqualified workload statement measured", res)
	}
	if res.Improvement <= 0 {
		t.Fatalf("improvement %.2f: want the category index to lower the plan cost",
			res.Improvement)
	}
	var path string
	if err := pool.QueryRow(t.Context(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "pg_catalog, pg_temp" {
		t.Fatalf("session search_path after the what-if = %q, want the pin kept", path)
	}
}

// A statement only pg_sage's own role path would resolve is unplannable.
func TestHypoPGNeverPlansOnPgSagesOwnPath(t *testing.T) {
	pool := appPathHypoPool(t)
	admin, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(t.Context(),
		"CREATE TABLE sage_own.only_sage (id int, category int)"); err != nil {
		t.Fatal(err)
	}
	h := NewHypoPG(pool, noopLog2)
	queries := []QueryInfo{
		{QueryID: 1, Calls: 10, TotalTimeMs: 100, Text: "SELECT id FROM items WHERE category = 7"},
		{QueryID: 2, Calls: 10, TotalTimeMs: 90,
			Text: "SELECT id FROM only_sage WHERE category = 7"},
	}
	rec := Recommendation{DDL: "CREATE INDEX ON items (category)",
		AffectedQueryIDs: []int64{1, 2}}
	res, err := h.Validate(t.Context(), rec, queries)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Measured != 1 || res.Failed != 1 || len(res.UnplannedQueryIDs) != 1 ||
		res.UnplannedQueryIDs[0] != 2 {
		t.Fatalf("result %+v: want only_sage's statement unplanned", res)
	}
}
