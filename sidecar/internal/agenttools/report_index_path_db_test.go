package agenttools

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: indexValid is one read on one pool.

// security-01c-r: a packet's CREATE INDEX on an unqualified table names no
// schema; pg_sage's pool is pinned to pg_catalog, pg_temp, so the index is
// looked up on the application's search_path, where the deploy created it.
func TestIndexValidResolvesAnUnqualifiedIndexOnTheApplicationPath(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "report_index_path")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	for _, sql := range []string{
		"CREATE TABLE public.orders (a int)",
		"CREATE INDEX idx_app ON public.orders (a)",
		"CREATE SCHEMA off_path",
		"CREATE TABLE off_path.orders (a int)",
		"CREATE INDEX idx_off ON off_path.orders (a)",
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
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tools := New(pool, Options{})
	for _, c := range []struct {
		ref  indexRef
		want bool
	}{
		{indexRef{Name: "idx_app"}, true},                     // public: on the path
		{indexRef{Schema: "public", Name: "idx_app"}, true},   // qualified
		{indexRef{Name: "idx_off"}, false},                    // not on the path
		{indexRef{Schema: "off_path", Name: "idx_off"}, true}, // qualified
		{indexRef{Name: "idx_missing"}, false},
		{indexRef{Schema: "public", Name: "orders"}, false}, // a table, not an index
	} {
		got, err := tools.indexValid(ctx, c.ref)
		if err != nil {
			t.Fatalf("indexValid(%s): %v", c.ref, err)
		}
		if got != c.want {
			t.Errorf("indexValid(%s) = %v, want %v", c.ref, got, c.want)
		}
	}
}
