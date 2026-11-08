//go:build cgo

package sqlast

import (
	"strings"
	"testing"
)

// security-01: a CREATE INDEX must name its table's schema, and a call in
// its key expressions or predicate may name only pg_catalog among the
// protected schemas. A bare call is accepted: the executor's pinned
// search_path (pg_catalog, pg_temp) binds it to pg_catalog, and PostgreSQL
// never looks up functions in pg_temp. Whether a qualified call into any
// other schema binds an extension's member is decided against the catalog
// before the build (executor.checkIndexFunctions).
func TestCheckIndexStmtRefusals(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE INDEX CONCURRENTLY i ON t (a)":                         "schema-qualified",
		"CREATE UNIQUE INDEX i ON ONLY t (a)":                          "schema-qualified",
		"CREATE INDEX i ON public.t ((sage.f(a)))":                     `protected schema "sage"`,
		"CREATE INDEX i ON public.t ((SAGE.f(a)))":                     `protected schema "sage"`,
		`CREATE INDEX i ON public.t (("sage".f(a)))`:                   `protected schema "sage"`,
		"CREATE INDEX i ON public.t (a) WHERE information_schema.x(a)": "protected schema",
		"CREATE INDEX i ON public.t ((lower(pg_toast.f(a))))":          "protected schema",
		"CREATE INDEX i ON public.t ((db.pg_toast.f(a)))":              "protected schema",
	} {
		err := Check(sql, testRules)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want a refusal mentioning %q", sql, err, want)
		}
	}
}

// Happy path, boundary and nil/empty: qualified targets with no calls,
// bare calls, pg_catalog calls (also as the grammar writes EXTRACT) and
// calls into an extension's schema pass the parse-tree layer.
func TestCheckIndexStmtAccepts(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY i ON public.t (a)",
		"CREATE INDEX i ON public.t ((lower(a)), (pg_catalog.upper(b)))",
		"CREATE INDEX i ON public.t ((extract(year from d))) WHERE PG_CATALOG.length(c) > 1",
		"CREATE INDEX i ON public.t USING gin ((ext.f(a)) gin_trgm_ops)",
	} {
		if err := Check(sql, testRules); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}
