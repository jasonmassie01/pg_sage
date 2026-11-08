package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrency test: each resolution is one read-only catalog
// transaction per statement, and the DDL it feeds runs under the DDL lease.

// setupOpclassTable creates public.sp_oc_t with text and int columns and
// pg_trgm in public (the layout most installs use).
func setupOpclassTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	ensurePgTrgmInPublic(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.sp_oc_t;
		CREATE TABLE public.sp_oc_t (email text, n int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.sp_oc_t")
	})
}

// indexOpclass reports the schema and name of the operator class an
// index's first key uses, and whether that class belongs to an extension.
func indexOpclass(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string,
) (schema, name string, member bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT n.nspname, c.opcname, EXISTS (
		SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid =
		'pg_catalog.pg_opclass'::pg_catalog.regclass AND d.objid = c.oid AND d.deptype = 'e')
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_opclass c ON c.oid = i.indclass[0]
		JOIN pg_catalog.pg_namespace n ON n.oid = c.opcnamespace
		WHERE i.indexrelid = pg_catalog.to_regclass($1)`, index).Scan(&schema, &name, &member)
	if err != nil {
		t.Fatalf("read the operator class of %s: %v", index, err)
	}
	return schema, name, member
}

// createPlainOpclass creates a btree operator class for text outside any
// extension, as a superuser could, and drops it (and its implicit family)
// at cleanup.
func createPlainOpclass(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	sql := "CREATE OPERATOR CLASS " + name + ` FOR TYPE text USING btree AS
		OPERATOR 1 <, OPERATOR 2 <=, OPERATOR 3 =, OPERATOR 4 >=, OPERATOR 5 >,
		FUNCTION 1 bttextcmp(text, text)`
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("create operator class %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DROP OPERATOR CLASS IF EXISTS "+name+" USING btree CASCADE")
		_, _ = pool.Exec(cctx, "DROP OPERATOR FAMILY IF EXISTS "+name+" USING btree CASCADE")
	})
}

// Happy path (the regression this file pins): pg_sage's pinned DDL builds
// an index whose operator class an extension installed in public defines,
// written bare as optimizer and LLM DDL write it, on both the CONCURRENTLY
// and the transactional path, and binds the extension's own class.
func TestPinnedDDLResolvesBareExtensionOpclass(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	if err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_cic
		ON public.sp_oc_t USING gin (email gin_trgm_ops)`, time.Minute); err != nil {
		t.Fatalf("bare gin_trgm_ops, CONCURRENTLY: %v", err)
	}
	if err := ExecInTransaction(ctx, pool, `CREATE INDEX sp_oc_tx
		ON public.sp_oc_t USING gist (email gist_trgm_ops)`, time.Minute); err != nil {
		t.Fatalf("bare gist_trgm_ops, in a transaction: %v", err)
	}
	for index, want := range map[string]string{
		"public.sp_oc_cic": "gin_trgm_ops", "public.sp_oc_tx": "gist_trgm_ops"} {
		schema, name, member := indexOpclass(t, ctx, pool, index)
		if schema != "public" || name != want || !member {
			t.Errorf("%s uses %s.%s (extension member %t), want pg_trgm's public.%s",
				index, schema, name, member, want)
		}
	}
	var path string
	if err := pool.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil ||
		path == executorSearchPath {
		t.Fatalf("pool session search_path = %q, %v; the pin leaked", path, err)
	}
}

// Security boundary: a superuser-made operator class in public that shadows
// a pg_catalog name never wins; pg_catalog's class is bound, as it was on
// the role's default path.
func TestPinnedDDLKeepsCatalogOpclassOverShadow(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	createPlainOpclass(t, ctx, pool, "public.text_pattern_ops")
	if err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_shadow
		ON public.sp_oc_t (email text_pattern_ops)`, time.Minute); err != nil {
		t.Fatalf("bare text_pattern_ops: %v", err)
	}
	if schema, name, _ := indexOpclass(t, ctx, pool, "public.sp_oc_shadow"); schema !=
		"pg_catalog" || name != "text_pattern_ops" {
		t.Fatalf("index uses %s.%s, want pg_catalog.text_pattern_ops", schema, name)
	}
}

// Invalid input: an operator class outside every extension is refused
// before anything runs, with an error that says why; written with its
// schema, it is the author's explicit choice and builds.
func TestPinnedDDLRefusesNonExtensionOpclass(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	createPlainOpclass(t, ctx, pool, "public.sp_oc_plain_ops")
	err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_plain
		ON public.sp_oc_t (email sp_oc_plain_ops)`, time.Minute)
	if !errors.Is(err, ErrUnresolvedOpclass) ||
		!strings.Contains(err.Error(), "not part of an extension") ||
		!strings.Contains(err.Error(), "public") {
		t.Fatalf("non-extension operator class error = %v, want ErrUnresolvedOpclass", err)
	}
	if def := liveIndexDef(t, ctx, pool, "public.sp_oc_plain"); def != "" {
		t.Fatalf("refused statement built %q", def)
	}
	if err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_plain_q
		ON public.sp_oc_t (email public.sp_oc_plain_ops)`, time.Minute); err != nil {
		t.Fatalf("qualified operator class: %v", err)
	}
}

// Boundary: resolution is by access method and extension membership, not
// by the role's search_path. btree_gin's gin int4_ops lives in a schema the
// role does not search, and shares its name with pg_catalog's btree
// int4_ops; the gin index binds btree_gin's.
func TestPinnedDDLResolvesExtensionOutsideSearchPath(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	schema := ensureBtreeGinOffPath(t, ctx, pool)
	if err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_gin
		ON public.sp_oc_t USING gin (n int4_ops)`, time.Minute); err != nil {
		t.Fatalf("bare int4_ops for gin: %v", err)
	}
	got, name, member := indexOpclass(t, ctx, pool, "public.sp_oc_gin")
	if got != schema || name != "int4_ops" || !member {
		t.Fatalf("gin index uses %s.%s (member %t), want btree_gin's %s.int4_ops",
			got, name, member, schema)
	}
	if err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_btree
		ON public.sp_oc_t (n int4_ops)`, time.Minute); err != nil {
		t.Fatalf("bare int4_ops for btree: %v", err)
	}
	if got, _, _ := indexOpclass(t, ctx, pool, "public.sp_oc_btree"); got != "pg_catalog" {
		t.Fatalf("btree index uses %s.int4_ops, want pg_catalog's", got)
	}
}

// Security boundary: membership counts only in the extension's own schema.
// A class a superuser added to btree_gin from another schema (ALTER
// EXTENSION ... ADD) is not the extension's own and is refused bare.
func TestPinnedDDLRefusesMemberOutsideExtensionSchema(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	extSchema := ensureBtreeGinOffPath(t, ctx, pool)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS sp_oc_side"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS sp_oc_side CASCADE")
	})
	createPlainOpclass(t, ctx, pool, "sp_oc_side.sp_oc_added_ops")
	if _, err := pool.Exec(ctx, "ALTER EXTENSION btree_gin ADD OPERATOR CLASS "+
		"sp_oc_side.sp_oc_added_ops USING btree"); err != nil {
		t.Fatalf("add the class to btree_gin (in %s): %v", extSchema, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ALTER EXTENSION btree_gin DROP "+
			"OPERATOR CLASS sp_oc_side.sp_oc_added_ops USING btree")
	})
	err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_added
		ON public.sp_oc_t (email sp_oc_added_ops)`, time.Minute)
	if !errors.Is(err, ErrUnresolvedOpclass) || !strings.Contains(err.Error(), "sp_oc_side") {
		t.Fatalf("member outside its extension's schema: %v, want ErrUnresolvedOpclass", err)
	}
	if def := liveIndexDef(t, ctx, pool, "public.sp_oc_added"); def != "" {
		t.Fatalf("refused statement built %q", def)
	}
}

// ensureBtreeGinOffPath returns the schema btree_gin is installed in,
// installing it into sp_oc_ext (not on any role's search_path) when it is
// absent, and removing what it installed at cleanup.
func ensureBtreeGinOffPath(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT n.nspname
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'btree_gin'), '')`).Scan(&schema); err != nil {
		t.Fatalf("check btree_gin: %v", err)
	}
	if schema != "" {
		return schema
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sp_oc_ext;
		CREATE EXTENSION btree_gin SCHEMA sp_oc_ext`); err != nil {
		t.Fatalf("install btree_gin (contrib is in every CI image): %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = pool.Exec(cctx, "DROP TABLE IF EXISTS public.sp_oc_t")
		_, _ = pool.Exec(cctx, "DROP EXTENSION IF EXISTS btree_gin")
		_, _ = pool.Exec(cctx, "DROP SCHEMA IF EXISTS sp_oc_ext")
	})
	return "sp_oc_ext"
}

// Nil/empty: an operator class no schema defines is PostgreSQL's to report
// (42704), not a resolution verdict, and nothing is built.
func TestPinnedDDLUnknownOpclassIsPostgresError(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	err := ExecConcurrently(ctx, pool, `CREATE INDEX CONCURRENTLY sp_oc_unknown
		ON public.sp_oc_t USING gin (email sp_oc_no_such_ops)`, time.Minute)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42704" || errors.Is(err, ErrUnresolvedOpclass) {
		t.Fatalf("unknown operator class error = %v, want undefined_object (42704)", err)
	}
	if def := liveIndexDef(t, ctx, pool, "public.sp_oc_unknown"); def != "" {
		t.Fatalf("failed statement built %q", def)
	}
}

// Error propagation: the catalog read honours the caller's context, and a
// failed read refuses the statement instead of running it bare.
func TestPinnedDDLOpclassLookupHonoursContext(t *testing.T) {
	pool, ctx := requireDB(t)
	setupOpclassTable(t, ctx, pool)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := poolOpclassLookup(pool)(cancelled, "gin_trgm_ops", "gin"); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("lookup with a cancelled context = %v, want context.Canceled", err)
	}
	err := ExecConcurrently(cancelled, pool, `CREATE INDEX CONCURRENTLY sp_oc_cancel
		ON public.sp_oc_t USING gin (email gin_trgm_ops)`, time.Minute)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ExecConcurrently = %v, want context.Canceled", err)
	}
	if def := liveIndexDef(t, ctx, pool, "public.sp_oc_cancel"); def != "" {
		t.Fatalf("cancelled statement built %q", def)
	}
}
