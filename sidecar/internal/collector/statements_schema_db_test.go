package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/extschema"
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

	assertSchemaReads(t, ctx, schemaCollector(pool, "ext"))
	// An empty startup schema (pg_stat_statements absent at startup) is
	// followed by the capability probe, which finds the extension in ext
	// (security-01c-q; the unqualified reads failed here before).
	assertSchemaReads(t, ctx, schemaCollector(pool, ""))

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
	assertWorkloadRoleTime(t, ctx, c)
}

// assertWorkloadRoleTime: the workload_sessions step (market-08) reads
// per-role statement time through the configured schema too.
func assertWorkloadRoleTime(t *testing.T, ctx context.Context, c *Collector) {
	t.Helper()
	schema := c.cfg.StatementsSchema
	ws, err := c.collectWorkloadSessions(ctx)
	if err != nil {
		t.Fatalf("schema %q: collectWorkloadSessions: %v", schema, err)
	}
	for _, r := range ws.Roles {
		if r.Calls > 0 && r.P99EstimateMs >= 0 {
			return
		}
	}
	t.Errorf("schema %q: no role statement time in %+v", schema, ws.Roles)
}

// Boundary (security-01c-q): an extension created after startup is read
// from the next capability refresh on, not before; until then a read is
// the undefined_table error of an absent extension.
func TestStatementsCreatedAfterStartupFollowTheRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, pool := schemaPool(t, ctx)
	if _, err := admin.Exec(ctx, "DROP EXTENSION pg_stat_statements"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	c := schemaCollector(pool, "")
	c.refreshCapabilities(ctx)
	if _, err := c.collectQueries(ctx); !errors.Is(err, extschema.ErrExtensionAbsent) {
		t.Fatalf("collectQueries without the extension: %v, want absent", err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION pg_stat_statements SCHEMA ext"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.collectQueries(ctx); !errors.Is(err, extschema.ErrExtensionAbsent) {
		t.Fatalf("collectQueries before the refresh: %v, want still absent", err)
	}
	c.refreshCapabilities(ctx)
	if got := c.statementsSchema(); got != "ext" {
		t.Fatalf("schema after the refresh = %q, want ext", got)
	}
	if _, err := c.collectQueries(ctx); err != nil {
		t.Fatalf("collectQueries after the refresh: %v", err)
	}
}

// State transitions (security-01c-q): two databases of a fleet, A with
// the extension in public and B in ext; each collector follows its own
// database's schema, and B's never names public's view.
func TestFleetCollectorsReadTheirOwnSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, poolB := schemaPool(t, ctx)
	poolA, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "collpgsspublic"))
	if err != nil {
		t.Fatalf("connect A: %v", err)
	}
	t.Cleanup(poolA.Close)
	for _, sql := range []string{"DROP EXTENSION IF EXISTS pg_stat_statements",
		"CREATE EXTENSION pg_stat_statements SCHEMA public"} {
		if _, err := poolA.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	a, b := schemaCollector(poolA, ""), schemaCollector(poolB, "")
	rec := &sqlRecorder{}
	b.onCatalogQuery = rec.hook
	for name, c := range map[string]*Collector{"public": a, "ext": b} {
		c.refreshCapabilities(ctx)
		if got := c.statementsSchema(); got != name {
			t.Fatalf("collector of the %s database reads schema %q", name, got)
		}
		if _, err := c.collectQueries(ctx); err != nil {
			t.Fatalf("%s: collectQueries: %v", name, err)
		}
	}
	if n := rec.count(`"public"."pg_stat_statements"`); n != 0 || rec.count(`"ext".`) == 0 {
		t.Fatalf("B sent %d reads of public's view and %d of ext's, want 0 and some",
			n, rec.count(`"ext".`))
	}
}
