package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration (real PostgreSQL 14 and 17, security-01): a second role
// plants public.lower(varchar) IMMUTABLE, which beats pg_catalog.lower(text)
// for a varchar argument on the default search_path, and the workload
// filters on lower(email). The optimizer's admitted DDL names pg_catalog,
// so even a build on the application's own path (HypoPG's, or an operator
// copying the DDL) never binds the planted function.

// attackTable creates public.<name>(email varchar, name text, d timestamp)
// and drops it after the test.
func attackTable(t *testing.T, pool *pgxpool.Pool) (string, TableContext) {
	t.Helper()
	name := volName("sage_sp_attack")
	volExec(t, pool, "CREATE TABLE public."+name+
		" (email varchar(100), name text, d timestamp, tz timestamptz)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+name)
	})
	tc := TableContext{Schema: "public", Table: name, Columns: []ColumnInfo{
		{Name: "email"}, {Name: "name"}, {Name: "d"}, {Name: "tz"}}}
	return name, tc
}

// functionDeps lists the functions an index has pg_depend rows for.
// pg_catalog functions are pinned and never recorded, so any entry at all
// is a function outside pg_catalog.
func functionDeps(t *testing.T, pool *pgxpool.Pool, index string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT n.nspname || '.' || p.proname || '(' ||
		       pg_catalog.pg_get_function_identity_arguments(p.oid) || ')'
		  FROM pg_catalog.pg_depend d
		  JOIN pg_catalog.pg_proc p ON p.oid = d.refobjid
		  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		 WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
		   AND d.objid = $1::pg_catalog.regclass
		   AND d.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
		 ORDER BY 1`, index)
	if err != nil {
		t.Fatalf("read pg_depend for %s: %v", index, err)
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			t.Fatal(err)
		}
		deps = append(deps, dep)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return deps
}

func indexDef(t *testing.T, pool *pgxpool.Pool, index string) string {
	t.Helper()
	var def string
	err := pool.QueryRow(context.Background(),
		"SELECT pg_catalog.pg_get_indexdef($1::pg_catalog.regclass)", index).Scan(&def)
	if err != nil {
		t.Fatalf("pg_get_indexdef(%s): %v", index, err)
	}
	return def
}

func plantPublicLower(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	plantAsAttacker(t, pool, "public", "CREATE FUNCTION public.lower(varchar) "+
		"RETURNS text IMMUTABLE LANGUAGE sql AS $$SELECT 'planted'::text$$")
}

// Attack (fails at 61152ef3, where the admitted DDL was the bare
// lower(email)): the control index built from the bare DDL on the default
// path binds the planted function; the admitted DDL does not.
func TestSearchPathAttack_AdmittedDDLNeverBindsPlanted(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	plantPublicLower(t, pool)
	ctx := context.Background()
	bare := "CREATE INDEX " + table + "_bare ON public." + table + " ((lower(email)))"
	volExec(t, pool, bare)
	if deps := functionDeps(t, pool, "public."+table+"_bare"); len(deps) != 1 ||
		deps[0] != "public.lower(character varying)" {
		t.Fatalf("precondition: the bare DDL on the default path binds %q, "+
			"want the planted public.lower", deps)
	}
	// The control index depends on the planted function; drop it so the
	// attacker's cleanup can drop the function.
	volExec(t, pool, "DROP INDEX public."+table+"_bare")
	rec := Recommendation{Table: "public." + table, DDL: "CREATE INDEX CONCURRENTLY " +
		table + "_adm ON public." + table + " ((lower(email)))"}
	got, ok, reason := v.ValidateAndQualify(ctx, rec, tc)
	if !ok {
		t.Fatalf("lower(email) refused: %s", reason)
	}
	if !strings.Contains(got.DDL, "((pg_catalog.lower(email)))") {
		t.Fatalf("admitted DDL %q does not name pg_catalog.lower", got.DDL)
	}
	volExec(t, pool, got.DDL)
	index := "public." + table + "_adm"
	if deps := functionDeps(t, pool, index); len(deps) != 0 {
		t.Fatalf("index built from the admitted DDL depends on %q, want none", deps)
	}
	// pg_catalog.lower(text) needs the varchar cast; the planted
	// lower(varchar) would deparse without it.
	def := indexDef(t, pool, index)
	if strings.Contains(def, "public.lower") || !strings.Contains(def, "lower((email)::text)") {
		t.Fatalf("pg_get_indexdef %q: want pg_catalog.lower(text), never public.lower", def)
	}
	if got.FindingIdentifier() != rec.FindingIdentifier() {
		t.Errorf("qualification changed the finding identity: %q -> %q",
			rec.FindingIdentifier(), got.FindingIdentifier())
	}
}

// Happy path: a pg_catalog.lower(text) expression index on varchar, with
// nothing planted, is admitted, qualified and builds.
func TestSearchPathAttack_CatalogLowerStillRecommended(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY " + table + "_ok ON public." +
		table + " ((LOWER(email))) WHERE lower(name) <> ''"}
	got, ok, reason := v.ValidateAndQualify(context.Background(), rec, tc)
	want := "CREATE INDEX CONCURRENTLY " + table + "_ok ON public." + table +
		" ((pg_catalog.LOWER(email))) WHERE pg_catalog.lower(name) <> ''"
	if !ok || got.DDL != want {
		t.Fatalf("ok=%t reason=%q ddl=%q\nwant %q", ok, reason, got.DDL, want)
	}
	volExec(t, pool, got.DDL)
	if def := indexDef(t, pool, "public."+table+"_ok"); !strings.Contains(def, "lower") {
		t.Fatalf("index definition %q", def)
	}
}

// Invalid input: a qualified call to the planted function and a call to
// a function no trusted schema holds are refused with a distinguishable
// reason, and nothing is rewritten.
func TestSearchPathAttack_PlantedAndUnknownRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	plantPublicLower(t, pool)
	for keys, want := range map[string]string{
		"((public.lower(email)))":     "function_schema_not_allowed: public.lower",
		"((evil_sage_fn_xyz(email)))": "function_schema_not_allowed: evil_sage_fn_xyz",
	} {
		rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY i ON public." + table + " " + keys}
		got, ok, reason := v.ValidateAndQualify(context.Background(), rec, tc)
		if ok || !strings.HasPrefix(reason, want) || got.DDL != rec.DDL {
			t.Errorf("%s: ok=%t reason=%q ddl=%q, want refused %q, DDL unchanged",
				keys, ok, reason, got.DDL, want)
		}
	}
}

// Boundary: an extension function binds and is named by its extension's
// schema only while that extension is allowlisted; an overload another
// role plants beside it makes the schema untrusted.
func TestSearchPathAttack_ExtensionSchemaQualified(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	schema := extensionSchema(t, pool, "fuzzystrmatch")
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY i_" + table + " ON public." + table +
		" ((soundex(name)))"}
	got, ok, reason := v.ValidateAndQualify(context.Background(), rec, tc)
	if !ok || !strings.Contains(got.DDL, "(("+schema+".soundex(name)))") {
		t.Fatalf("ok=%t reason=%q ddl=%q, want soundex named by %s", ok, reason, got.DDL, schema)
	}
	volExec(t, pool, got.DDL)
	v.extensions = []string{"pg_trgm"}
	if _, ok, reason = v.ValidateAndQualify(context.Background(), rec, tc); ok ||
		!strings.Contains(reason, "function_schema_not_allowed") {
		t.Fatalf("off the allowlist: ok=%t reason=%q, want refused", ok, reason)
	}
	v.extensions = expressionIndexExtensions
	plantAsAttacker(t, pool, schema, "CREATE FUNCTION "+schema+".soundex(varchar) "+
		"RETURNS text IMMUTABLE LANGUAGE sql AS $$SELECT 'planted'::text$$")
	_, ok, reason = v.ValidateAndQualify(context.Background(), rec, tc)
	if ok || !strings.Contains(reason, "function_schema_not_allowed") {
		t.Fatalf("planted overload beside the extension: ok=%t reason=%q, want refused",
			ok, reason)
	}
}

// Boundary: date_trunc mixes IMMUTABLE (timestamp) and STABLE
// (timestamptz) overloads in pg_catalog, so it is refused on either column.
func TestSearchPathAttack_MixedOverloadSetRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	for _, col := range []string{"d", "tz"} {
		rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY i ON public." + table +
			" ((date_trunc('day', " + col + ")))"}
		if _, ok, reason := v.ValidateAndQualify(context.Background(), rec, tc); ok ||
			!strings.Contains(reason, "not IMMUTABLE") {
			t.Errorf("%s: ok=%t reason=%q, want not IMMUTABLE", col, ok, reason)
		}
	}
}

// Nil/empty: an index with no expressions is admitted byte for byte.
func TestSearchPathAttack_PlainIndexUnchanged(t *testing.T) {
	v, pool := volatilityValidator(t)
	table, tc := attackTable(t, pool)
	rec := Recommendation{DDL: "CREATE INDEX CONCURRENTLY i ON public." + table +
		" (email) INCLUDE (name) WHERE d IS NOT NULL"}
	got, ok, reason := v.ValidateAndQualify(context.Background(), rec, tc)
	if !ok || reason != "" || got.DDL != rec.DDL {
		t.Fatalf("ok=%t reason=%q ddl=%q, want unchanged", ok, reason, got.DDL)
	}
}
