package optimizer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Integration (real PostgreSQL 14 and 17, security-01b): the expression-
// index volatility check counts only functions in pg_catalog and members
// of allowlisted extensions, so a function a low-privilege role plants in
// public (or in an extension's schema) never makes an index admissible.

var volSeq atomic.Int64

// volName is a name no other test (or package fixture) uses; roles are
// cluster-wide, so the pid keeps parallel packages apart.
func volName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), volSeq.Add(1))
}

func volatilityValidator(t *testing.T) (*Validator, *pgxpool.Pool) {
	t.Helper()
	pool := connectTestDB(t)
	t.Cleanup(pool.Close)
	return NewValidator(pool, fnTestOptimizerConfig(), noopLog), pool
}

func volExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// plantAsAttacker runs stmts as a fresh NOLOGIN role that has only USAGE
// and CREATE on schema (PG14's default PUBLIC CREATE on public, granted
// explicitly so PG15+ behaves the same). Its objects are dropped after.
func plantAsAttacker(t *testing.T, pool *pgxpool.Pool, schema string, stmts ...string) {
	t.Helper()
	role := volName("sage_vol_attacker")
	volExec(t, pool, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+role)
		_, _ = pool.Exec(ctx, "DROP ROLE "+role)
	})
	volExec(t, pool, "GRANT USAGE, CREATE ON SCHEMA "+schema+" TO "+role)
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			return err
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("plant as %s: %v", role, err)
	}
}

// extensionSchema installs ext into a fresh schema, dropped after the test.
func extensionSchema(t *testing.T, pool *pgxpool.Pool, exts ...string) string {
	t.Helper()
	schema := volName("sage_volext")
	volExec(t, pool, "CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, ext := range exts {
			_, _ = pool.Exec(ctx, "DROP EXTENSION IF EXISTS "+ext+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	for _, ext := range exts {
		volExec(t, pool, "CREATE EXTENSION "+ext+" SCHEMA "+schema)
	}
	return schema
}

func checkVol(v *Validator, ctx context.Context, ddl string) (bool, string) {
	return v.checkExpressionVolatility(ctx, Recommendation{DDL: ddl})
}

func wantRefused(t *testing.T, ok bool, reason string, wantIn ...string) {
	t.Helper()
	if ok {
		t.Fatalf("admitted, want refused (reason must contain %q)", wantIn)
	}
	for _, w := range wantIn {
		if !strings.Contains(reason, w) {
			t.Errorf("reason %q does not contain %q", reason, w)
		}
	}
}

func wantAdmitted(t *testing.T, ok bool, reason string) {
	t.Helper()
	if !ok || reason != "" {
		t.Fatalf("ok=%t reason=%q, want admitted with no reason", ok, reason)
	}
}

// Attack (fails at 61152ef3): the only IMMUTABLE overload of the name is
// one a second role planted in public.
func TestVolatility_PlantedPublicOnlyFunctionRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	fn := volName("sage_planted")
	plantAsAttacker(t, pool, "public", "CREATE FUNCTION public."+fn+
		"(varchar) RETURNS text IMMUTABLE LANGUAGE sql AS $$SELECT $1::text$$")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t (("+fn+"(email)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", fn)
}

// Attack (fails at 61152ef3): a planted IMMUTABLE overload must not make a
// STABLE builtin admissible; public.now(integer) is never counted.
func TestVolatility_PlantedOverloadNeverCounted(t *testing.T) {
	v, pool := volatilityValidator(t)
	plantAsAttacker(t, pool, "public", "CREATE FUNCTION public.now(integer) "+
		"RETURNS timestamptz IMMUTABLE LANGUAGE sql AS $$SELECT 'epoch'::timestamptz$$")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((created_at > now()))")
	wantRefused(t, ok, reason, "now", "not IMMUTABLE")
}

// A planted VOLATILE public.lower(varchar) would make every-overload
// IMMUTABLE fail if it were counted; lower(email) still passes on
// pg_catalog.lower(text), so public.lower is not in the set.
func TestVolatility_PlantedPublicLowerNotCounted(t *testing.T) {
	v, pool := volatilityValidator(t)
	plantAsAttacker(t, pool, "public", "CREATE FUNCTION public.lower(varchar) "+
		"RETURNS text VOLATILE LANGUAGE sql AS $$SELECT 'x'::text$$")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))")
	wantAdmitted(t, ok, reason)
}

// Happy path: lower() on text resolves to pg_catalog.lower(text), whose
// overloads are all IMMUTABLE; case folding does not change that.
func TestVolatility_CatalogLowerAdmitted(t *testing.T) {
	v, _ := volatilityValidator(t)
	for _, ddl := range []string{
		"CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))",
		"CREATE INDEX CONCURRENTLY i ON public.t ((LOWER(email)))",
		"CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)), (upper(name)))",
	} {
		ok, reason := checkVol(v, context.Background(), ddl)
		if !ok || reason != "" {
			t.Errorf("%s: ok=%t reason=%q, want admitted", ddl, ok, reason)
		}
	}
}

// Boundary (fails at 61152ef3): pg_catalog.date_trunc mixes IMMUTABLE
// (timestamp) and STABLE (timestamptz) overloads, so it is refused until
// security-01 resolves the call to one overload by OID.
func TestVolatility_MixedOverloadSetRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	var kinds string
	if err := pool.QueryRow(context.Background(), `SELECT string_agg(DISTINCT
		provolatile::text, '' ORDER BY provolatile::text) FROM pg_catalog.pg_proc
		WHERE proname = 'date_trunc' AND pronamespace = 'pg_catalog'::regnamespace`).
		Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "is" {
		t.Fatalf("precondition: date_trunc volatility kinds = %q, want \"is\"", kinds)
	}
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t (date_trunc('day', created_at))")
	wantRefused(t, ok, reason, "date_trunc", "not IMMUTABLE")
}

// Boundary: an extension function passes only when its extension is on
// the allowlist (fuzzystrmatch is; seg, whose seg_size is IMMUTABLE, is
// not; fails at 61152ef3).
func TestVolatility_ExtensionFunctionNeedsAllowlist(t *testing.T) {
	v, pool := volatilityValidator(t)
	extensionSchema(t, pool, "fuzzystrmatch", "seg")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((soundex(name)))")
	wantAdmitted(t, ok, reason)
	ok, reason = checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((seg_size(span)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", "seg_size")
}

// Attack (fails at 61152ef3): a function planted in an allowlisted
// extension's own schema is not a member of the extension and is never
// counted. Extensions usually live in public, so trusting the schema
// alone would re-open the public.lower attack.
func TestVolatility_NonMemberInExtensionSchemaRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	schema := extensionSchema(t, pool, "fuzzystrmatch")
	fn := volName("sage_planted_ext")
	plantAsAttacker(t, pool, schema, "CREATE FUNCTION "+schema+"."+fn+
		"(text) RETURNS text IMMUTABLE LANGUAGE sql AS $$SELECT $1$$")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t (("+fn+"(name)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", fn)
}

// Invalid input (fails at 61152ef3): a name with no function at all is
// refused rather than left to PostgreSQL.
func TestVolatility_UnknownFunctionRefused(t *testing.T) {
	v, _ := volatilityValidator(t)
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((no_such_function_xyz(a)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", "no_such_function_xyz")
}

// Invalid input: PostgreSQL folds unquoted names, so NOW() is now() and
// must be looked up, not skipped by the lowercase regex (ParseIndexDDL
// folds the expressions; this pins it).
func TestVolatility_UppercaseCallIsChecked(t *testing.T) {
	v, _ := volatilityValidator(t)
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((created_at > NOW()))")
	wantRefused(t, ok, reason, "now", "not IMMUTABLE")
}

// SQL keywords followed by "(" are grammar, not function calls, and stay
// admissible (COALESCE, IN, AND, OR, NOT cannot bind a user function).
func TestVolatility_KeywordsAdmitted(t *testing.T) {
	v, _ := volatilityValidator(t)
	for _, ddl := range []string{
		"CREATE INDEX CONCURRENTLY i ON public.t (status) WHERE status IN ('a', 'b')",
		"CREATE INDEX CONCURRENTLY i ON public.t (status) WHERE status in ('a') " +
			"and (amount > 0) or not (archived)",
		"CREATE INDEX CONCURRENTLY i ON public.t ((coalesce(a, b)))",
		"CREATE INDEX CONCURRENTLY i ON public.t ((nullif(a, '')), (greatest(a, b)))",
	} {
		ok, reason := checkVol(v, context.Background(), ddl)
		if !ok || reason != "" {
			t.Errorf("%s: ok=%t reason=%q, want admitted", ddl, ok, reason)
		}
	}
}

// Error propagation (fails at 61152ef3): a failing catalog read refuses
// the candidate with the error; it never admits it.
func TestVolatility_CatalogReadErrorRefuses(t *testing.T) {
	v, _ := volatilityValidator(t)
	injected := errors.New("injected catalog failure")
	ctx := catalogread.WithBeforeStatement(context.Background(),
		func(context.Context, pgx.Tx) error { return injected })
	ok, reason := checkVol(v, ctx,
		"CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))")
	wantRefused(t, ok, reason, "lower", "injected catalog failure")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ok, reason = checkVol(v, canceled,
		"CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))")
	wantRefused(t, ok, reason, "lower", "context canceled")
}

// The lookup runs in a catalogread transaction (fails at 61152ef3): read
// only, with the safety statement and lock timeouts set locally.
func TestVolatility_LookupIsBounded(t *testing.T) {
	v, _ := volatilityValidator(t)
	var readOnly, stmt, lock string
	ctx := catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only'),
				current_setting('statement_timeout'), current_setting('lock_timeout')`).
				Scan(&readOnly, &stmt, &lock)
		})
	ok, reason := checkVol(v, ctx, "CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))")
	wantAdmitted(t, ok, reason)
	want := catalogread.Default()
	if readOnly != "on" {
		t.Errorf("transaction_read_only = %q, want on", readOnly)
	}
	if stmt != fmt.Sprintf("%dms", want.Statement.Milliseconds()) &&
		stmt != fmt.Sprintf("%ds", int(want.Statement.Seconds())) {
		t.Errorf("statement_timeout = %q, want %v", stmt, want.Statement)
	}
	if lock == "0" || lock == "" {
		t.Errorf("lock_timeout = %q, want the safety lock timeout %v", lock, want.Lock)
	}
}

// Attack (review): a schema-qualified call to a planted function is not
// rescued by pg_catalog's function of the same name; the qualified schema
// is what is looked up.
func TestVolatility_QualifiedPlantedCallRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	plantAsAttacker(t, pool, "public", "CREATE FUNCTION public.lower(varchar) "+
		"RETURNS text IMMUTABLE LANGUAGE sql AS $$SELECT 'x'::text$$")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((public.lower(email)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", "public.lower")
	ok, reason = checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((pg_catalog.lower(email)))")
	wantAdmitted(t, ok, reason)
}

// Boundary (review): an allowlisted extension's function called through
// its own schema passes; the same name qualified by another schema does
// not.
func TestVolatility_QualifiedExtensionCall(t *testing.T) {
	v, pool := volatilityValidator(t)
	schema := extensionSchema(t, pool, "fuzzystrmatch")
	ok, reason := checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t (("+schema+".soundex(name)))")
	wantAdmitted(t, ok, reason)
	ok, reason = checkVol(v, context.Background(),
		"CREATE INDEX CONCURRENTLY i ON public.t ((public.soundex(name)))")
	wantRefused(t, ok, reason, "function_schema_not_allowed", "public.soundex")
}

// Attack (second review): names that Go normalization folds or collapses
// onto a pg_catalog name are refused. PostgreSQL keeps İ and a no-break
// space as part of the identifier, so these DDLs call the planted
// functions, not pg_catalog.initcap, lower or the SIMILAR grammar.
func TestVolatility_LexerDivergencePlantedRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	plantAsAttacker(t, pool, "public",
		`CREATE FUNCTION public."İnitcap"(varchar) RETURNS text IMMUTABLE `+
			`LANGUAGE sql AS $$SELECT 'x'::text$$`,
		"CREATE FUNCTION public.\"lower\u00a0\"(varchar) RETURNS text IMMUTABLE "+
			"LANGUAGE sql AS $$SELECT 'x'::text$$",
		`CREATE FUNCTION public.similar(varchar) RETURNS text IMMUTABLE `+
			`LANGUAGE sql AS $$SELECT 'x'::text$$`)
	for _, keys := range []string{
		"((İNITCAP(email)))",
		"((lower\u00a0(email)))",
		"((similar(email)))",
	} {
		ok, reason := checkVol(v, context.Background(),
			"CREATE INDEX CONCURRENTLY i ON public.t "+keys)
		if ok || !strings.Contains(reason, "function_schema_not_allowed") {
			t.Errorf("%q: ok=%t reason=%q, want function_schema_not_allowed",
				keys, ok, reason)
		}
	}
	ok, reason := checkVol(v, context.Background(), "CREATE INDEX CONCURRENTLY i "+
		"ON public.t ((lower(city))) WHERE lower(city) = 'zürich' AND email like ('a%')")
	wantAdmitted(t, ok, reason)
}
