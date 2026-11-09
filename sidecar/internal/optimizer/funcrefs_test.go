package optimizer

import (
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: IndexFunctionCalls is a pure function of its
// argument and keeps no state.

func callNames(refs []FuncRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	return out
}

// Happy path: calls in the key list and the predicate are found in order,
// with the offset of their name as written.
func TestIndexFunctionCalls_KeysAndPredicate(t *testing.T) {
	ddl := "CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)), (Upper(name))) " +
		"WHERE pg_catalog.length(code) > 2;"
	refs, err := IndexFunctionCalls(ddl)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(callNames(refs), ","); got != "lower,upper,pg_catalog.length" {
		t.Fatalf("calls = %q", got)
	}
	for _, r := range refs {
		written := ddl[r.Start:]
		if !strings.HasPrefix(strings.ToLower(written), r.String()+"(") {
			t.Errorf("%s: offset %d points at %q", r, r.Start, written[:12])
		}
	}
}

// Nil/empty: an index with no expressions has no calls; a statement the
// scanner cannot read is an error, not an empty list.
func TestIndexFunctionCalls_NoExpressions(t *testing.T) {
	for _, ddl := range []string{
		"CREATE INDEX i ON public.t (a, b)",
		"CREATE INDEX i ON public.t USING gin (doc jsonb_path_ops) INCLUDE (c)",
		"CREATE INDEX i ON public.t (a) WITH (fillfactor = 90) WHERE b IS NOT NULL",
		"CREATE INDEX i ON public.t ((a + 1) DESC NULLS LAST)",
	} {
		refs, err := IndexFunctionCalls(ddl)
		if err != nil || len(refs) != 0 {
			t.Errorf("%s: refs=%v err=%v, want none", ddl, callNames(refs), err)
		}
	}
	for _, ddl := range []string{"", "DROP INDEX public.i", "CREATE INDEX i ON public.t (a"} {
		if _, err := IndexFunctionCalls(ddl); err == nil {
			t.Errorf("%q: no error", ddl)
		}
	}
}

// Boundary: what is grammar, not a call, is never reported: keywords
// followed by "(", infix LIKE with a parenthesised pattern, an operator
// class with parameters, cast type modifiers, and text inside literals.
func TestIndexFunctionCalls_GrammarIsNotACall(t *testing.T) {
	for _, keys := range []string{
		"((coalesce(a, b)), (nullif(a, '')))",
		"(a) WHERE a IN ('x', 'y') AND b like ('a%') AND NOT (c) AND exists (d)",
		"(c gist_trgm_ops(siglen=64), (d) gist_trgm_ops (siglen=32))",
		"((a::numeric(10,2)), (b::character varying(20)), (cast(c as varchar(3))))",
		"((d::timestamp(3) without time zone))",
		"(a) WHERE b = 'lower(x)' AND c = E'it\\'s upper(y)' AND d = 'q''s(z)'",
		"((t.a), (1e5 + 2))",
	} {
		refs, err := IndexFunctionCalls("CREATE INDEX i ON public.t " + keys)
		if err != nil || len(refs) != 0 {
			t.Errorf("%s: refs=%v err=%v, want none", keys, callNames(refs), err)
		}
	}
}

// Boundary: quoted names keep their case, unquoted ASCII folds, and a
// name after a keyword operator is a call.
func TestIndexFunctionCalls_NamesAsPostgresReadsThem(t *testing.T) {
	refs, err := IndexFunctionCalls(`CREATE INDEX i ON public.t ` +
		`(("Lower"(a)), (public."F"(b)), (LOWER (c))) WHERE x AND fé(d) OR not g(e)`)
	if err != nil {
		t.Fatal(err)
	}
	want := `Lower,public.F,lower,fé,g`
	if got := strings.Join(callNames(refs), ","); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// Grammar forms are reported but flagged, so they are never qualified.
func TestIndexFunctionCalls_GrammarForms(t *testing.T) {
	refs, err := IndexFunctionCalls("CREATE INDEX i ON public.t " +
		"((extract(year from d)), (substring(s from 1 for 2)), (pg_catalog.substring(s, 1)))")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || !refs[0].Grammar() || !refs[1].Grammar() || refs[2].Grammar() {
		t.Fatalf("refs = %+v, want extract and substring grammar, the qualified call not", refs)
	}
}

// Invalid input: text whose reading depends on more than the scanner
// knows is refused with ErrUnreadableExpression.
func TestIndexFunctionCalls_UnreadableRefused(t *testing.T) {
	for _, keys := range []string{
		"((lower/**/(a)))",
		"((lower(a) -- c\n))",
		"((a || $$x$$))",
		`((U&"l\006fwer"(a)))`,
		`((a || 'back\slash'))`,
	} {
		_, err := IndexFunctionCalls("CREATE INDEX i ON public.t " + keys)
		if !errors.Is(err, ErrUnreadableExpression) {
			t.Errorf("%s: err = %v, want ErrUnreadableExpression", keys, err)
		}
	}
}
