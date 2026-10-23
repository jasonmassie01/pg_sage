//go:build cgo

package changereview

import "testing"

// CHECK-FND-04 (cgo build): the content hash is built from parse trees,
// so comments, whitespace and keyword case do not change it, while an
// edited statement (including a changed literal or type modifier) does.

func TestContentHashASTIgnoresFormatting(t *testing.T) {
	a := []SQLFile{{Path: "001.sql", Content: `-- add the note column
ALTER TABLE public.orders ADD COLUMN note text;
CREATE INDEX CONCURRENTLY orders_note_idx ON public.orders (note);`}}
	b := []SQLFile{{Path: "002_renamed.sql", Content: `/* reviewed */ alter table
	public.orders   add column note TEXT ;  create index concurrently orders_note_idx
	on public.orders(note) -- trailing
	;`}}
	if ContentHash(a, "") != ContentHash(b, "") {
		t.Fatal("comments, whitespace, case or the file name changed the AST content hash")
	}
	keys, ast := StatementKeys(a[0].Content)
	if !ast || len(keys) != 2 {
		t.Fatalf("StatementKeys = %v (ast %v), want 2 parse-tree keys", keys, ast)
	}
}

func TestContentHashASTSeesEdits(t *testing.T) {
	base := []SQLFile{{Content: "ALTER TABLE public.orders ALTER COLUMN code TYPE varchar(20);"}}
	for name, sql := range map[string]string{
		"type modifier": "ALTER TABLE public.orders ALTER COLUMN code TYPE varchar(40);",
		"column":        "ALTER TABLE public.orders ALTER COLUMN name TYPE varchar(20);",
		"extra stmt": "ALTER TABLE public.orders ALTER COLUMN code TYPE varchar(20);" +
			" SET lock_timeout = '2s';",
	} {
		if ContentHash(base, "") == ContentHash([]SQLFile{{Content: sql}}, "") {
			t.Errorf("%s edit kept the content hash", name)
		}
	}
	lt1 := []SQLFile{{Content: "SET lock_timeout = '2s'; ALTER TABLE t ADD COLUMN a int;"}}
	lt2 := []SQLFile{{Content: "SET lock_timeout = '0'; ALTER TABLE t ADD COLUMN a int;"}}
	if ContentHash(lt1, "") == ContentHash(lt2, "") {
		t.Fatal("a changed literal kept the content hash; a carried verdict would be unsafe")
	}
}

func TestStatementKeysFallBackOnParseError(t *testing.T) {
	keys, ast := StatementKeys("ALTER TABLE orders ADD COLUMN;")
	if ast || len(keys) != 1 {
		t.Fatalf("keys %v ast %v, want one text-mode key", keys, ast)
	}
}
