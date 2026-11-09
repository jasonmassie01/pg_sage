package workload

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-04: the statement-to-table resolver shared by the optimizer's
// table context and the tuning agent's workload classification.

func rel(schema, name string) Relation { return Relation{Schema: schema, Name: name} }

func assertRefs(t *testing.T, query string, known map[string][]string, want ...Relation) {
	t.Helper()
	got := ReferencedTables(query, known)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("ReferencedTables(%q) = %v, want none", query, got)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReferencedTables(%q) = %v, want %v", query, got, want)
	}
}

// Happy path: an unqualified name in the only schema holding it (an app on
// search_path=app) resolves to that schema, not to public.
func TestReferencedTables_UnqualifiedResolvesToItsOnlySchema(t *testing.T) {
	known := map[string][]string{"orders": {"app"}}
	assertRefs(t, "SELECT id FROM orders WHERE customer_id = $1", known, rel("app", "orders"))
	assertRefs(t, "select * from ORDERS where id = $1", known, rel("app", "orders"))
}

// Boundary: a qualified name keeps its schema even when public holds the
// same name; an unknown schema is not guessed.
func TestReferencedTables_QualifiedKeepsItsSchema(t *testing.T) {
	known := map[string][]string{"orders": {"public", "app"}}
	assertRefs(t, "SELECT * FROM app.orders WHERE id = $1", known, rel("app", "orders"))
	assertRefs(t, "SELECT * FROM public.orders", known, rel("public", "orders"))
	assertRefs(t, "SELECT * FROM app . orders o", known, rel("app", "orders"))
	assertRefs(t, "SELECT * FROM billing.orders", known)
}

// Known limit, pinned: a name held by two schemas resolves to public when
// public is one of them (the statement's role and search_path are not in
// pg_stat_statements' snapshot), and to nothing when neither is public.
func TestReferencedTables_AmbiguousNameRule(t *testing.T) {
	assertRefs(t, "SELECT * FROM orders", map[string][]string{"orders": {"app", "public"}},
		rel("public", "orders"))
	assertRefs(t, "SELECT * FROM orders", map[string][]string{"orders": {"app", "billing"}})
}

// Invalid input: quoted identifiers keep their case (and doubled quotes),
// bare ones fold to lower case like PostgreSQL folds them.
func TestReferencedTables_QuotedIdentifiers(t *testing.T) {
	known := map[string][]string{"Items": {"App"}, `we"ird`: {"public"}}
	assertRefs(t, `SELECT * FROM "App"."Items" WHERE "Id" = $1`, known, rel("App", "Items"))
	assertRefs(t, `SELECT * FROM App.Items`, known)
	assertRefs(t, `SELECT * FROM "we""ird" w`, known, rel("public", `we"ird`))
}

// Invalid input: every relation of a FROM list counts, with or without
// aliases, and ONLY is not a relation.
func TestReferencedTables_FromListAndOnly(t *testing.T) {
	known := map[string][]string{"a": {"public"}, "b": {"public"}, "c": {"s"},
		"t": {"public"}}
	assertRefs(t, "SELECT * FROM a, b x, s.c AS y WHERE a.id = x.id", known,
		rel("public", "a"), rel("public", "b"), rel("s", "c"))
	assertRefs(t, "SELECT * FROM ONLY t WHERE id = $1", known, rel("public", "t"))
	assertRefs(t, "UPDATE ONLY t SET v = $1", known, rel("public", "t"))
}

// Invalid input: a CTE name is not a table; the CTE's own FROM still is.
func TestReferencedTables_CTENameIsNotATable(t *testing.T) {
	known := map[string][]string{"orders": {"app"}}
	assertRefs(t, "WITH recent AS (SELECT * FROM orders WHERE ts > $1) "+
		"SELECT * FROM recent r", known, rel("app", "orders"))
}

// Invalid input: a CTE named like a real table is the CTE, as in
// PostgreSQL; column lists, RECURSIVE, [NOT] MATERIALIZED and several CTEs
// all define names. A qualified reference still names the table.
func TestReferencedTables_CTEShadowsTableOfItsName(t *testing.T) {
	known := map[string][]string{"orders": {"public"}, "recent": {"public"},
		"t": {"public"}, "u": {"public"}, "items": {"public"}}
	assertRefs(t, "WITH recent AS (SELECT * FROM orders WHERE ts > $1) "+
		"SELECT * FROM recent r", known, rel("public", "orders"))
	assertRefs(t, "WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t "+
		"WHERE n < 5), u AS NOT MATERIALIZED (SELECT * FROM items) "+
		"SELECT * FROM t, u, orders", known, rel("public", "items"), rel("public", "orders"))
	assertRefs(t, "WITH recent AS MATERIALIZED (SELECT 1) SELECT * FROM public.recent",
		known, rel("public", "recent"))
	assertRefs(t, "WITH x AS (DELETE FROM orders RETURNING *) INSERT INTO recent "+
		"SELECT * FROM x", known, rel("public", "orders"), rel("public", "recent"))
}

// Boundary: WITH that defines no CTE (TIME ZONE, ORDINALITY, storage
// options) shadows nothing.
func TestReferencedTables_WithThatIsNoCTE(t *testing.T) {
	known := map[string][]string{"time": {"public"}, "ordinality": {"public"},
		"t": {"public"}}
	assertRefs(t, "SELECT ts::timestamp with time zone FROM time", known,
		rel("public", "time"))
	assertRefs(t, "SELECT * FROM t, unnest($1) WITH ORDINALITY AS o(v, n) "+
		"JOIN ordinality ON true", known, rel("public", "ordinality"), rel("public", "t"))
	assertRefs(t, "CREATE INDEX i ON t (v) WITH (fillfactor = 70)", known)
	assertRefs(t, `WITH "t" (a) AS`, known)
	assertRefs(t, "WITH t AS (SELECT * FROM", known)
	// A parenthesis inside a quoted identifier does not end the CTE body.
	assertRefs(t, `WITH r AS (SELECT ")" FROM ordinality) SELECT * FROM r, t`, known,
		rel("public", "ordinality"), rel("public", "t"))
}

// Invalid input: string literals and comments never read as relations.
func TestReferencedTables_LiteralsAndCommentsIgnored(t *testing.T) {
	known := map[string][]string{"orders": {"public"}, "x": {"public"}, "y": {"public"},
		"z": {"public"}}
	assertRefs(t, "SELECT 'FROM x' FROM orders -- JOIN y\n WHERE n = 'it''s' /* FROM z */",
		known, rel("public", "orders"))
	assertRefs(t, "SELECT 'a FROM x' AS v", known)
}

// Happy path: JOIN, INSERT INTO, UPDATE and subqueries all count; the
// result is sorted and has no duplicates.
func TestReferencedTables_AllClausesSortedAndDeduplicated(t *testing.T) {
	known := map[string][]string{"a": {"public"}, "b": {"public"}, "c": {"public"},
		"d": {"public"}}
	assertRefs(t, "INSERT INTO d SELECT * FROM c JOIN b ON b.id = c.id "+
		"WHERE c.id IN (SELECT id FROM a) AND c.v IN (SELECT v FROM b b2)", known,
		rel("public", "a"), rel("public", "b"), rel("public", "c"), rel("public", "d"))
	assertRefs(t, "UPDATE a SET v = b.v FROM b WHERE a.id = b.id", known,
		rel("public", "a"), rel("public", "b"))
}

// Boundary: a name is matched whole, never by prefix; functions and
// catalogs absent from the snapshot are left out.
func TestReferencedTables_WholeNamesOnly(t *testing.T) {
	known := map[string][]string{"orders": {"app"}}
	assertRefs(t, "SELECT * FROM orders_archive", known)
	assertRefs(t, "SELECT * FROM generate_series(1, 3) g", known)
	assertRefs(t, "SELECT * FROM pg_stat_activity", known)
}

// Invalid input: malformed SQL yields what can be read, never a panic.
func TestReferencedTables_MalformedSQL(t *testing.T) {
	known := map[string][]string{"orders": {"public"}}
	for _, q := range []string{`SELECT * FROM "Ord`, "SELECT * FROM", "SELECT * FROM ,",
		"SELECT * FROM 123", "/* FROM orders", "'FROM orders", `SELECT * FROM ""`,
		"SELECT * FROM app."} {
		assertRefs(t, q, known)
	}
	assertRefs(t, "SELECT * FROM orders, ", known, rel("public", "orders"))
}

// Nil/empty: no query, no snapshot tables, nothing referenced.
func TestReferencedTables_NilAndEmpty(t *testing.T) {
	known := map[string][]string{"orders": {"public"}}
	if got := ReferencedTables("", known); got != nil {
		t.Fatalf("empty query = %v, want nil", got)
	}
	if got := ReferencedTables("SELECT * FROM orders", nil); got != nil {
		t.Fatalf("nil known = %v, want nil", got)
	}
	if got := ReferencedTables("SELECT * FROM orders", map[string][]string{}); got != nil {
		t.Fatalf("empty known = %v, want nil", got)
	}
	if got := ReferencedTables("SELECT 1", known); got != nil {
		t.Fatalf("no relation = %v, want nil", got)
	}
}

func TestKnownTables(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "orders"}, {SchemaName: "app", RelName: "orders"},
		{SchemaName: "App", RelName: "Items"}}}
	want := map[string][]string{"orders": {"public", "app"}, "Items": {"App"}}
	if got := KnownTables(snap); !reflect.DeepEqual(got, want) {
		t.Fatalf("KnownTables = %v, want %v", got, want)
	}
	if got := KnownTables(&collector.Snapshot{}); got == nil || len(got) != 0 {
		t.Fatalf("empty snapshot = %#v, want an empty map", got)
	}
	if got := KnownTables(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil snapshot = %#v, want an empty map", got)
	}
}

// Literals and comments become spaces of the same length (newlines kept);
// quoted identifiers stay.
func TestBlankLiteralsAndComments(t *testing.T) {
	in := "SELECT 'x;y', \"a'b\" -- c;\nFROM t /* d; */"
	want := "SELECT " + strings.Repeat(" ", 5) + ", \"a'b\" " + strings.Repeat(" ", 5) +
		"\nFROM t " + strings.Repeat(" ", 8)
	if got := BlankLiteralsAndComments(in); got != want {
		t.Fatalf("BlankLiteralsAndComments = %q, want %q", got, want)
	}
	if got := BlankLiteralsAndComments(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := BlankLiteralsAndComments("'open"); got != "     " {
		t.Fatalf("unterminated literal = %q, want all blank", got)
	}
}

func TestReadIdentAndQualified(t *testing.T) {
	name, next, err := ReadIdent(`"A""B" rest`, 0)
	if err != nil || name != `A"B` || next != 6 {
		t.Fatalf("quoted = %q %d %v", name, next, err)
	}
	if name, next, err = ReadIdent("Foo_1$x", 0); err != nil || name != "foo_1$x" || next != 7 {
		t.Fatalf("bare = %q %d %v", name, next, err)
	}
	for _, bad := range []string{"", `""`, `"open`, "1abc", " x"} {
		if _, _, err := ReadIdent(bad, 0); err == nil {
			t.Errorf("ReadIdent(%q) succeeded, want an error", bad)
		}
	}
	schema, rel, next, err := ReadQualified(`"App" . "Items" x`, 0)
	if err != nil || schema != "App" || rel != "Items" || next != 15 {
		t.Fatalf("qualified = %q %q %d %v", schema, rel, next, err)
	}
	if schema, rel, _, err = ReadQualified("orders o", 0); err != nil || schema != "" ||
		rel != "orders" {
		t.Fatalf("unqualified = %q %q %v", schema, rel, err)
	}
	if _, _, _, err = ReadQualified("app.", 0); err == nil {
		t.Fatal("a schema without a name parsed")
	}
}

// Concurrent access: the resolver keeps no state and only reads known, so
// parallel callers sharing one map agree. -race is unavailable here; the
// assertions still catch crossed results.
func TestReferencedTables_ParallelCallersShareKnown(t *testing.T) {
	known := map[string][]string{"orders": {"app"}, "items": {"public"}}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, want := "SELECT * FROM orders", rel("app", "orders")
			if i%2 == 1 {
				q, want = "SELECT * FROM items", rel("public", "items")
			}
			if got := ReferencedTables(q, known); len(got) != 1 || got[0] != want {
				t.Errorf("caller %d: %v, want %v", i, got, want)
			}
		}()
	}
	wg.Wait()
}

// No state-transition tests: the resolver is a pure function.
// No integration test here: the resolver reads no database; the optimizer's
// table_context_db_test.go runs it on real pg_stat_statements text.
