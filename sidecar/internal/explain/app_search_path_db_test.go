//go:build integration

package explain

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c: workload EXPLAIN plans the application's statements on
// the application's search_path (the database's setting), never on
// pg_sage's session path: pinned (pg_catalog, pg_temp) it would not find
// the application's tables, and pg_sage's own role setting is not the
// application's.

// appPathDB creates a database whose application tables live in schema
// app (the database's search_path) and whose pg_sage role path names
// schema sage_own, which holds a table the application does not see.
func appPathDB(t *testing.T) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "explain_app_path")
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if err := schema.Bootstrap(t.Context(), admin); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var db string
	if err := admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("database name: %v", err)
	}
	ident := pgx.Identifier{db}.Sanitize()
	for _, sql := range []string{
		"CREATE SCHEMA app",
		"CREATE TABLE app.orders (id int PRIMARY KEY, note text)",
		"CREATE SCHEMA sage_own",
		"CREATE TABLE sage_own.only_sage (id int)",
		"ALTER DATABASE " + ident + " SET search_path = app, public",
		"ALTER ROLE CURRENT_USER IN DATABASE " + ident + " SET search_path = sage_own",
	} {
		if _, err := admin.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return dsn
}

// explainPool opens a one-connection pool on dsn; pinned sessions run
// under search_path = pg_catalog, pg_temp as pg_sage's own do.
func explainPool(t *testing.T, dsn string, pinned bool) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	if pinned {
		cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
			return pc.Exec(ctx, "SET search_path = pg_catalog, pg_temp").Close()
		}
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestExplainPlansOnTheApplicationPathUnderAPinnedSession(t *testing.T) {
	pool := explainPool(t, appPathDB(t), true)
	ex := New(pool, testExplainConfig(), noopLog)
	for _, query := range []string{
		"SELECT note FROM orders WHERE id = 1",
		"SELECT note FROM orders WHERE id = $1", // PREPARE + EXPLAIN EXECUTE
	} {
		res, err := ex.Explain(t.Context(), ExplainRequest{Query: query, PlanOnly: true})
		if err != nil {
			t.Fatalf("Explain(%q): %v", query, err)
		}
		if !strings.Contains(string(res.PlanJSON), `"orders"`) {
			t.Fatalf("Explain(%q) plan %s does not scan orders", query, res.PlanJSON)
		}
	}
	var path string
	if err := pool.QueryRow(t.Context(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatalf("show search_path: %v", err)
	}
	if path != "pg_catalog, pg_temp" {
		t.Fatalf("session search_path after EXPLAIN = %q, want the pin kept", path)
	}
}

// pg_sage's own role path (sage_own) is not the application's, so a
// table only it holds does not resolve, pinned session or not.
func TestExplainNeverInheritsPgSagesSessionPath(t *testing.T) {
	dsn := appPathDB(t)
	for _, pinned := range []bool{false, true} {
		ex := New(explainPool(t, dsn, pinned), testExplainConfig(), noopLog)
		_, err := ex.Explain(t.Context(),
			ExplainRequest{Query: "SELECT id FROM only_sage", PlanOnly: true})
		if err == nil || !strings.Contains(err.Error(), "only_sage") {
			t.Fatalf("pinned=%v: Explain = %v, want only_sage not to resolve", pinned, err)
		}
	}
}
