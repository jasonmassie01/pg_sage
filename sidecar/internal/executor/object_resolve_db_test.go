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

// Integration (real PostgreSQL 14 and 17, security-01 review): a qualified
// type or operator binds its schema whatever the search_path, so a domain
// whose CHECK calls a planted function, or a planted operator, would run
// as the table owner during the build. The planted function raises, so a
// build that reached it fails with "planted ran" instead of pg_sage's
// refusal.

// plantObjects creates, as plantLowerAttack's attacker role, a domain
// public.sp_evil_dom checked by a planted function and an operator
// public.=== backed by one.
func plantObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	plantLowerAttack(t, ctx, pool)
	role := fmt.Sprintf("sage_sp_attacker_%d", os.Getpid())
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, s := range []string{
		"SET ROLE " + role,
		"CREATE FUNCTION public.sp_evil_chk(varchar) RETURNS boolean IMMUTABLE " +
			"LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'planted ran'; END$$",
		"CREATE DOMAIN public.sp_evil_dom AS varchar CHECK (public.sp_evil_chk(VALUE))",
		"CREATE FUNCTION public.sp_evil_eq(varchar, varchar) RETURNS boolean IMMUTABLE " +
			"LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'planted ran'; END$$",
		"CREATE OPERATOR public.=== (LEFTARG = varchar, RIGHTARG = varchar, " +
			"FUNCTION = public.sp_evil_eq)",
		"RESET ROLE",
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		for _, s := range []string{
			"DROP OPERATOR IF EXISTS public.=== (varchar, varchar)",
			"DROP FUNCTION IF EXISTS public.sp_evil_eq(varchar, varchar)",
			"DROP DOMAIN IF EXISTS public.sp_evil_dom",
			"DROP FUNCTION IF EXISTS public.sp_evil_chk(varchar)",
		} {
			if _, err := pool.Exec(context.Background(), s); err != nil {
				t.Errorf("cleanup %q: %v", s, err)
			}
		}
	})
}

// Attack: a cast to the planted domain, a typed literal of it and the
// planted operator are refused before the build on both DDL paths.
func TestObjectSchemaPlantedRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	plantObjects(t, ctx, pool)
	exprs := map[string]string{
		"cast":          "((email::public.sp_evil_dom))",
		"cast_as":       "((CAST(email AS public.sp_evil_dom)))",
		"typed_literal": "(email) WHERE email <> public.sp_evil_dom 'x'",
		"operator":      "(id) WHERE email OPERATOR(public.===) 'x'",
	}
	execs := map[string]func(context.Context, *pgxpool.Pool, string, time.Duration,
		...DDLOption) error{"concurrently": ExecConcurrently, "transaction": ExecInTransaction}
	for name, keys := range exprs {
		for path, exec := range execs {
			index := "sp_obj_" + name + "_" + path
			sql := "CREATE INDEX " + index + " ON public.sp_attack_t " + keys
			if path == "concurrently" {
				sql = strings.Replace(sql, "CREATE INDEX ", "CREATE INDEX CONCURRENTLY ", 1)
			}
			err := exec(ctx, pool, sql, time.Minute, WithLockTimeout(5000))
			if !errors.Is(err, ErrFunctionSchemaNotAllowed) ||
				!strings.Contains(err.Error(), "public.") {
				t.Errorf("%s/%s: err = %v, want ErrFunctionSchemaNotAllowed", name, path, err)
			}
			if fnIndexBuilt(t, ctx, pool, "public."+index) {
				t.Errorf("%s/%s: index built", name, path)
			}
		}
	}
}

// Happy path: an extension's own type and operator, in the extension's
// schema, build.
func TestObjectSchemaExtensionMembersBuild(t *testing.T) {
	pool, ctx := requireDB(t)
	plantLowerAttack(t, ctx, pool)
	schema := fmt.Sprintf("sp_obj_ext_%d", os.Getpid())
	for _, sql := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
		"CREATE SCHEMA " + schema,
		"CREATE EXTENSION citext SCHEMA " + schema,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	sql := "CREATE INDEX CONCURRENTLY sp_obj_citext ON public.sp_attack_t ((email::" +
		schema + ".citext)) WHERE email::" + schema + ".citext OPERATOR(" + schema +
		".=) 'a'::" + schema + ".citext"
	if err := ExecConcurrently(ctx, pool, sql, time.Minute); err != nil {
		t.Fatalf("extension type and operator: %v", err)
	}
	if !fnIndexBuilt(t, ctx, pool, "public.sp_obj_citext") {
		t.Fatal("index over the extension's type was not built")
	}
}

// Boundary: a domain whose CHECK calls only pg_catalog builds through a
// cast; a domain over the planted domain inherits its refusal.
func TestObjectSchemaDomainChain(t *testing.T) {
	pool, ctx := requireDB(t)
	plantObjects(t, ctx, pool)
	for _, sql := range []string{
		"CREATE DOMAIN public.sp_ok_dom AS varchar CHECK (pg_catalog.length(VALUE) < 200)",
		"CREATE DOMAIN public.sp_chain_dom AS public.sp_evil_dom",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP DOMAIN IF EXISTS public.sp_ok_dom, public.sp_chain_dom CASCADE")
	})
	if err := ExecConcurrently(ctx, pool, "CREATE INDEX CONCURRENTLY sp_obj_okdom ON "+
		"public.sp_attack_t ((email::public.sp_ok_dom))", time.Minute); err != nil {
		t.Fatalf("domain with a pg_catalog CHECK: %v", err)
	}
	err := ExecConcurrently(ctx, pool, "CREATE INDEX CONCURRENTLY sp_obj_chain ON "+
		"public.sp_attack_t ((email::public.sp_chain_dom))", time.Minute)
	if !errors.Is(err, ErrFunctionSchemaNotAllowed) || fnIndexBuilt(t, ctx, pool,
		"public.sp_obj_chain") {
		t.Fatalf("domain over the planted domain: err = %v, want refused before the build", err)
	}
}
