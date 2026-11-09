package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// pg_stat_statements created in a schema outside the search_path: the
// collector reads it through cfg.StatementsSchema (firstlook-08), as the
// startup checks resolved it from pg_extension.

func schemaPool(t *testing.T, ctx context.Context) (admin, pool *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "collpgssschema")
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	for _, stmt := range []string{
		"DROP EXTENSION IF EXISTS pg_stat_statements",
		"CREATE SCHEMA ext",
		"CREATE EXTENSION pg_stat_statements SCHEMA ext",
		`CREATE SCHEMA "o'dd ""x"""`,
		"CREATE TABLE public.fl08_marker (i int)",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path public: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "SELECT count(*) FROM fl08_marker"); err != nil {
		t.Fatalf("seed a statement: %v", err)
	}
	return admin, pool
}

func schemaCollector(pool *pgxpool.Pool, schema string) *Collector {
	cfg := testConfig()
	cfg.StatementsSchema = schema
	return New(pool, cfg, 170000, noopLog)
}

func TestCollectQueries_ExtensionOutsideSearchPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, pool := schemaPool(t, ctx)

	// Today's unqualified SQL cannot find the extension: the schema is
	// what makes the read work, not the fixture.
	if _, err := schemaCollector(pool, "").collectQueries(ctx); err == nil {
		t.Fatal("unqualified collectQueries succeeded; the fixture leaks the schema")
	}
	assertSchemaReads(t, ctx, schemaCollector(pool, "ext"))

	if _, err := admin.Exec(ctx,
		`ALTER EXTENSION pg_stat_statements SET SCHEMA "o'dd ""x"""`); err != nil {
		t.Fatalf("move extension: %v", err)
	}
	assertSchemaReads(t, ctx, schemaCollector(pool, `o'dd "x"`))
}

func assertSchemaReads(t *testing.T, ctx context.Context, c *Collector) {
	t.Helper()
	schema := c.cfg.StatementsSchema
	c.refreshCapabilities(ctx)
	if caps := c.Capabilities(); !caps.StatStatementsView || !caps.BlockIOTime {
		t.Errorf("schema %q: view or block time columns not found: %+v", schema, caps)
	}
	queries, err := c.collectQueries(ctx)
	if err != nil {
		t.Fatalf("schema %q: collectQueries: %v", schema, err)
	}
	found := false
	for _, q := range queries {
		found = found || strings.Contains(q.Query, "fl08_marker")
	}
	if !found {
		t.Errorf("schema %q: the seeded statement is missing from %d queries", schema,
			len(queries))
	}
	u, err := c.readStatStatementsUsage(ctx, true)
	if err != nil {
		t.Fatalf("schema %q: usage: %v", schema, err)
	}
	if u.Entries == 0 || u.Dealloc < 0 {
		t.Errorf("schema %q: usage %+v, want entries and a known dealloc", schema, *u)
	}
	if epoch := c.collectStatementsEpoch(ctx); epoch.IsZero() {
		t.Errorf("schema %q: statistics epoch unknown", schema)
	}
}
