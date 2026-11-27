package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration (real PostgreSQL 14 and 17, security-01): the pinned
// search_path makes a bare call bind pg_catalog, but a call that names its
// schema binds that schema. The executor refuses, before anything runs, a
// qualified call into a schema holding a function outside any extension
// (public.lower planted by another role) and a bare call pg_catalog lacks.

func fnIndexBuilt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, index string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.to_regclass($1) IS NOT NULL",
		index).Scan(&ok); err != nil {
		t.Fatalf("to_regclass(%s): %v", index, err)
	}
	return ok
}

// Attack: an explicitly qualified call to the planted function is refused
// on both DDL paths, so the planted function never runs as the owner.
func TestFunctionSchemaQualifiedPlantedRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	cases := []struct {
		name, index, sql string
		exec             func(context.Context, *pgxpool.Pool, string, time.Duration,
			...DDLOption) error
	}{
		{"concurrently", "public.sp_fn_cic", "CREATE INDEX CONCURRENTLY sp_fn_cic " +
			"ON public.sp_attack_t ((public.lower(email)))", ExecConcurrently},
		{"transaction", "public.sp_fn_tx", "CREATE INDEX sp_fn_tx " +
			"ON public.sp_attack_t ((public.lower(email)))", ExecInTransaction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plantLowerAttack(t, ctx, pool)
			err := tc.exec(ctx, pool, tc.sql, time.Minute, WithLockTimeout(5000))
			if !errors.Is(err, ErrFunctionSchemaNotAllowed) ||
				!strings.Contains(err.Error(), "public.lower") {
				t.Fatalf("err = %v, want ErrFunctionSchemaNotAllowed naming public.lower", err)
			}
			if fnIndexBuilt(t, ctx, pool, tc.index) {
				t.Fatalf("index %s was built", tc.index)
			}
		})
	}
}

// Error propagation: a bare call pg_catalog lacks fails at validation with
// pg_sage's reason, not PostgreSQL's "function does not exist".
func TestFunctionSchemaUnknownBareRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	plantLowerAttack(t, ctx, pool)
	err := ExecConcurrently(ctx, pool, "CREATE INDEX CONCURRENTLY sp_fn_evil ON "+
		"public.sp_attack_t ((sage_evil_fn_xyz(email)))", time.Minute)
	if !errors.Is(err, ErrFunctionSchemaNotAllowed) || strings.Contains(err.Error(), "42883") {
		t.Fatalf("err = %v, want ErrFunctionSchemaNotAllowed before PostgreSQL", err)
	}
}

// Happy path and boundary: pg_catalog.lower builds; a function of an
// extension installed in its own schema builds qualified; an overload
// another role plants beside it makes that schema refused.
func TestFunctionSchemaCatalogAndExtension(t *testing.T) {
	pool, ctx := requireDB(t)
	plantLowerAttack(t, ctx, pool)
	if err := ExecConcurrently(ctx, pool, "CREATE INDEX CONCURRENTLY sp_fn_cat ON "+
		"public.sp_attack_t ((pg_catalog.lower(email)))", time.Minute); err != nil {
		t.Fatalf("pg_catalog.lower: %v", err)
	}
	schema := fuzzystrmatchSchema(t, ctx, pool)
	sql := "CREATE INDEX CONCURRENTLY sp_fn_ext ON public.sp_attack_t ((" +
		schema + ".soundex(email)))"
	if err := ExecConcurrently(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("extension function: %v", err)
	}
	plantBesideExtension(t, ctx, pool, schema)
	err := ExecConcurrently(ctx, pool, strings.Replace(sql, "sp_fn_ext ", "sp_fn_ext2 ", 1),
		time.Minute)
	if !errors.Is(err, ErrFunctionSchemaNotAllowed) {
		t.Fatalf("planted overload beside the extension: err = %v, want refused", err)
	}
	if fnIndexBuilt(t, ctx, pool, "public.sp_fn_ext2") {
		t.Fatal("index built over the planted overload")
	}
}

// fuzzystrmatchSchema installs fuzzystrmatch into a fresh schema, dropped
// after the test.
func fuzzystrmatchSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	schema := fmt.Sprintf("sp_fn_ext_%d", os.Getpid())
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
		"CREATE SCHEMA " + schema,
		"CREATE EXTENSION fuzzystrmatch SCHEMA " + schema,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	return schema
}

// plantBesideExtension creates schema.soundex(varchar) as plantLowerAttack's
// attacker role, an overload beside fuzzystrmatch's own soundex(text).
func plantBesideExtension(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	schema string) {
	t.Helper()
	role := fmt.Sprintf("sage_sp_attacker_%d", os.Getpid())
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, s := range []string{
		"GRANT USAGE, CREATE ON SCHEMA " + schema + " TO " + role,
		"SET ROLE " + role,
		"CREATE FUNCTION " + schema + ".soundex(varchar) RETURNS text IMMUTABLE " +
			"LANGUAGE sql AS $$SELECT 'planted'::text$$",
		"RESET ROLE",
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}
