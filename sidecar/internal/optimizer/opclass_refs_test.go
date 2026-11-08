package optimizer

import (
	"errors"
	"testing"
)

// No concurrent access tests: IndexOpclasses is a pure function of its
// input string.

// refText is the text an OpclassRef's span covers in ddl.
func refText(t *testing.T, ddl string, r OpclassRef) string {
	t.Helper()
	if r.Start < 0 || r.End > len(ddl) || r.Start >= r.End {
		t.Fatalf("span [%d,%d) out of range for %q", r.Start, r.End, ddl)
	}
	return ddl[r.Start:r.End]
}

// Happy path: a bare extension operator class is found with the exact span
// of its name, and the access method comes from USING.
func TestIndexOpclassesFindsBareExtensionOpclass(t *testing.T) {
	ddl := "CREATE INDEX CONCURRENTLY i ON public.t USING gin (email gin_trgm_ops)"
	method, refs, err := IndexOpclasses(ddl)
	if err != nil {
		t.Fatalf("IndexOpclasses: %v", err)
	}
	if method != "gin" || len(refs) != 1 {
		t.Fatalf("method %q refs %+v, want gin and one operator class", method, refs)
	}
	r := refs[0]
	if r.Schema != "" || r.Name != "gin_trgm_ops" || refText(t, ddl, r) != "gin_trgm_ops" {
		t.Fatalf("ref = %+v (%q), want bare gin_trgm_ops", r, refText(t, ddl, r))
	}
}

// Every key is read with its decorations in any order; only keys that name
// an operator class produce a ref, in key order.
func TestIndexOpclassesMixedKeys(t *testing.T) {
	ddl := `CREATE INDEX i ON public.t USING btree (a text_pattern_ops, ` +
		`lower(b) varchar_pattern_ops DESC NULLS LAST, c COLLATE "C", d, ` +
		`e COLLATE "C" text_pattern_ops ASC)`
	method, refs, err := IndexOpclasses(ddl)
	if err != nil {
		t.Fatalf("IndexOpclasses: %v", err)
	}
	want := []string{"text_pattern_ops", "varchar_pattern_ops", "text_pattern_ops"}
	if method != "btree" || len(refs) != len(want) {
		t.Fatalf("method %q refs %+v, want btree and %v", method, refs, want)
	}
	for i, r := range refs {
		if r.Name != want[i] || refText(t, ddl, r) != want[i] || r.Schema != "" {
			t.Errorf("ref %d = %+v (%q), want bare %s", i, r, refText(t, ddl, r), want[i])
		}
	}
	if refs[0].Start >= refs[1].Start || refs[1].Start >= refs[2].Start {
		t.Errorf("refs not in key order: %+v", refs)
	}
}

// A qualified or quoted operator class keeps its schema and exact spelling;
// the span covers the qualifier too.
func TestIndexOpclassesQualifiedAndQuoted(t *testing.T) {
	ddl := `CREATE INDEX i ON public.t USING gin (a public.gin_trgm_ops, ` +
		`b "Ext"."My_Ops", c "Mixed_Ops")`
	_, refs, err := IndexOpclasses(ddl)
	if err != nil || len(refs) != 3 {
		t.Fatalf("refs %+v, %v; want 3", refs, err)
	}
	cases := []struct{ schema, name, text string }{
		{"public", "gin_trgm_ops", "public.gin_trgm_ops"},
		{"Ext", "My_Ops", `"Ext"."My_Ops"`},
		{"", "Mixed_Ops", `"Mixed_Ops"`},
	}
	for i, c := range cases {
		r := refs[i]
		if r.Schema != c.schema || r.Name != c.name || refText(t, ddl, r) != c.text {
			t.Errorf("ref %d = %+v (%q), want %+v", i, r, refText(t, ddl, r), c)
		}
	}
}

// Boundary: an operator class with parameters spans only its name;
// unquoted upper case folds like PostgreSQL while the span keeps the text.
func TestIndexOpclassesParametersAndCase(t *testing.T) {
	ddl := "CREATE INDEX i ON public.t USING gist (tsv tsvector_ops(siglen=32), " +
		"doc GIST_TRGM_OPS (siglen = 64))"
	method, refs, err := IndexOpclasses(ddl)
	if err != nil || method != "gist" || len(refs) != 2 {
		t.Fatalf("method %q refs %+v, %v", method, refs, err)
	}
	if refText(t, ddl, refs[0]) != "tsvector_ops" || refs[0].Name != "tsvector_ops" {
		t.Errorf("first ref = %+v (%q)", refs[0], refText(t, ddl, refs[0]))
	}
	if refText(t, ddl, refs[1]) != "GIST_TRGM_OPS" || refs[1].Name != "gist_trgm_ops" {
		t.Errorf("second ref = %+v (%q), want folded gist_trgm_ops", refs[1],
			refText(t, ddl, refs[1]))
	}
}

// Spans are offsets into the statement as given: leading whitespace, a
// trailing semicolon, line breaks, INCLUDE and WHERE do not shift them.
func TestIndexOpclassesSpansIgnoreSurroundingText(t *testing.T) {
	ddl := "  \n\tCREATE UNIQUE INDEX IF NOT EXISTS i\n ON ONLY public.t\n" +
		"  USING hnsw (emb vector_l2_ops)\n INCLUDE (id) WITH (m = 16) WHERE id > 0;\n"
	method, refs, err := IndexOpclasses(ddl)
	if err != nil || method != "hnsw" || len(refs) != 1 {
		t.Fatalf("method %q refs %+v, %v", method, refs, err)
	}
	if refText(t, ddl, refs[0]) != "vector_l2_ops" {
		t.Fatalf("span covers %q, want vector_l2_ops", refText(t, ddl, refs[0]))
	}
}

// Nil/empty: no USING means btree; a key list without operator classes, a
// string literal that looks like one, and INCLUDE columns produce no refs.
func TestIndexOpclassesNoOpclass(t *testing.T) {
	for _, ddl := range []string{
		"CREATE INDEX i ON public.t (a, b DESC)",
		"CREATE INDEX i ON public.t ((payload->>'gin_trgm_ops'))",
		"CREATE INDEX i ON public.t (coalesce(a, 'x,y'), b) INCLUDE (c)",
		`CREATE INDEX i ON public.t ("text_pattern_ops")`,
	} {
		method, refs, err := IndexOpclasses(ddl)
		if err != nil || method != "btree" || len(refs) != 0 {
			t.Errorf("%q: method %q refs %+v, %v; want btree and none", ddl, method, refs, err)
		}
	}
}

// Invalid input: anything that is not a readable CREATE INDEX is an error,
// never a guess.
func TestIndexOpclassesRejectsUnreadableStatements(t *testing.T) {
	for _, ddl := range []string{
		"",
		"DROP INDEX CONCURRENTLY public.i",
		"CREATE TABLE public.t (a int)",
		"CREATE INDEX i ON public.t USING gin (email gin_trgm_ops",
		"CREATE INDEX i ON public.t USING gin",
		"CREATE INDEX i ON public.t (a gin_trgm_ops gist_trgm_ops)",
	} {
		if _, refs, err := IndexOpclasses(ddl); err == nil {
			t.Errorf("%q: refs %+v and no error, want an error", ddl, refs)
		}
	}
	if _, _, err := IndexOpclasses("SELECT 1"); !errors.Is(err, errNotCreateIndex) {
		t.Errorf("SELECT error = %v, want errNotCreateIndex", err)
	}
}
