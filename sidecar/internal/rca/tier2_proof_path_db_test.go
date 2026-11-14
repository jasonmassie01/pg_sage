package rca

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: one proof transaction per incident on one
// pooled session; the pool's concurrency is selfmonitor's.

// security-01c-r: pg_sage's pools are pinned to pg_catalog, pg_temp, but a
// person runs Tier 2 SQL on the application's search_path. The proof
// resolves the SQL's unqualified names there, so a volatile public
// function that wins resolution for that person is refused, and an
// application table the SQL names without a schema is found.

// proofPathPool is a pg_sage-configured pool on a database whose public
// schema holds a volatile lower(name) (an exact match for lower(usename),
// so it wins over pg_catalog.lower(text) on any path that has public) and
// an orders table.
func proofPathPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "rca_proof_path")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	for _, sql := range []string{
		`CREATE FUNCTION public.lower(a name) RETURNS text LANGUAGE plpgsql VOLATILE
			AS $$BEGIN RETURN pg_catalog.lower(a::text); END$$`,
		"CREATE TABLE public.orders (id int)",
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixedInspect stands in for the parser (a no-cgo build has none).
func fixedInspect(q sqlast.ReadQuery) func(string) (sqlast.ReadQuery, error) {
	return func(string) (sqlast.ReadQuery, error) { return q, nil }
}

// vetOnProofTx runs the proof as vetTier2SQL does, on beginProofTx.
func vetOnProofTx(t *testing.T, pool *pgxpool.Pool, sql string, q sqlast.ReadQuery) Incident {
	t.Helper()
	ctx := context.Background()
	tx, err := beginProofTx(ctx, pool)
	if err != nil {
		t.Fatalf("beginProofTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inc := Incident{RecommendedSQL: sql}
	vetModelSQL(ctx, tx, fixedInspect(q), &inc, noopLog)
	return inc
}

func TestTier2ProofResolvesOnTheApplicationPath(t *testing.T) {
	pool := proofPathPool(t)
	catalog := sqlast.QualifiedName{Schema: "pg_catalog", Name: "pg_stat_activity"}
	shadowed := "SELECT lower(usename) FROM pg_catalog.pg_stat_activity"
	inc := vetOnProofTx(t, pool, shadowed, sqlast.ReadQuery{
		Functions: []sqlast.QualifiedName{{Name: "lower"}},
		Relations: []sqlast.QualifiedName{catalog}})
	if inc.RecommendedSQL != "" || !strings.Contains(withheldNote(inc), "volatile function") {
		t.Errorf("%s: SQL %q note %q, want withheld for the volatile public.lower(name)",
			shadowed, inc.RecommendedSQL, withheldNote(inc))
	}
	appTable := "SELECT id FROM orders"
	inc = vetOnProofTx(t, pool, appTable, sqlast.ReadQuery{
		Relations: []sqlast.QualifiedName{{Name: "orders"}}})
	if inc.RecommendedSQL != appTable || withheldNote(inc) != "" {
		t.Errorf("%s: SQL %q note %q, want kept (orders is on the application's path)",
			appTable, inc.RecommendedSQL, withheldNote(inc))
	}
	var path string
	if err := pool.QueryRow(context.Background(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != testdb.PinnedPath {
		t.Fatalf("search_path after the proof = %q, want the pin %q", path, testdb.PinnedPath)
	}
}
