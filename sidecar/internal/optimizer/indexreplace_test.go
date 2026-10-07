package optimizer

import "testing"

// A replacement (roadmap 2.3) is one action of two statements: the wider
// index's CREATE INDEX CONCURRENTLY and the DROP INDEX CONCURRENTLY of the
// index it subsumes; its undo re-creates the old index and drops the new
// one. Both pairs travel as text (finding, approval queue, card), so the
// approval binds to both statements.

const (
	replaceCreate = "CREATE INDEX CONCURRENTLY orders_status_created ON public.orders " +
		"(status, created_at)"
	replaceOldDef = "CREATE INDEX orders_status_idx ON public.orders USING btree (status)"
)

func TestIndexReplaceSQLRoundTrip(t *testing.T) {
	pair := IndexReplaceSQL(replaceCreate+";", `"public"."orders_status_idx"`)
	want := replaceCreate + ";\nDROP INDEX CONCURRENTLY \"public\".\"orders_status_idx\";"
	if pair != want {
		t.Fatalf("pair = %q\nwant %q", pair, want)
	}
	create, drop, ok := SplitIndexReplaceSQL(pair)
	if !ok || create != replaceCreate ||
		drop != `DROP INDEX CONCURRENTLY "public"."orders_status_idx"` {
		t.Fatalf("split = %q, %q, %v", create, drop, ok)
	}
	undo := IndexReplaceRollbackSQL(replaceOldDef, "public.orders_status_created")
	wantUndo := "CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders USING btree " +
		"(status);\nDROP INDEX CONCURRENTLY IF EXISTS public.orders_status_created;"
	if undo != wantUndo {
		t.Fatalf("undo = %q\nwant %q", undo, wantUndo)
	}
	recreate, dropNew, ok := SplitIndexReplaceSQL(undo)
	if !ok || recreate != "CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders "+
		"USING btree (status)" ||
		dropNew != "DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_created" {
		t.Fatalf("split undo = %q, %q, %v", recreate, dropNew, ok)
	}
}

func TestIndexReplaceRollbackSQLKeepsAConcurrentDefinition(t *testing.T) {
	def := "CREATE INDEX CONCURRENTLY a ON public.t USING btree (x)"
	got := IndexReplaceRollbackSQL(def, "public.b")
	want := def + ";\nDROP INDEX CONCURRENTLY IF EXISTS public.b;"
	if got != want {
		t.Fatalf("a definition already CONCURRENTLY is not rewritten twice: %q", got)
	}
}

func TestSplitIndexReplaceSQLRefusesOtherShapes(t *testing.T) {
	drop := "DROP INDEX CONCURRENTLY public.orders_status_idx"
	for name, sql := range map[string]string{
		"empty":            "",
		"whitespace":       " ;\n ; ",
		"one statement":    replaceCreate,
		"one with a semi":  replaceCreate + ";",
		"three statements": replaceCreate + ";\n" + drop + ";\n" + drop + ";",
		"drop first":       drop + ";\n" + replaceCreate + ";",
		"two creates":      replaceCreate + ";\n" + replaceCreate + ";",
		"two drops":        drop + ";\n" + drop + ";",
		"not concurrent create": "CREATE INDEX orders_x ON public.orders (status);\n" +
			drop + ";",
		"not concurrent drop": replaceCreate + ";\nDROP INDEX public.orders_status_idx;",
		"empty second":        replaceCreate + ";;",
		"trailing text":       replaceCreate + ";\n" + drop + "; SELECT 1",
	} {
		if a, b, ok := SplitIndexReplaceSQL(sql); ok {
			t.Fatalf("%s: split %q into %q, %q", name, sql, a, b)
		}
	}
}

func TestSplitIndexReplaceSQLIgnoresSemicolonsInLiteralsAndIdentifiers(t *testing.T) {
	create := `CREATE INDEX CONCURRENTLY "a;b" ON public.t (x) WHERE note <> 'x;y'`
	pair := create + ";\nDROP INDEX CONCURRENTLY public.old;"
	got, drop, ok := SplitIndexReplaceSQL(pair)
	if !ok || got != create || drop != "DROP INDEX CONCURRENTLY public.old" {
		t.Fatalf("split = %q, %q, %v", got, drop, ok)
	}
}
