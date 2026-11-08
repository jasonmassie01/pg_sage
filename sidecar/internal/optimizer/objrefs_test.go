package optimizer

import (
	"errors"
	"fmt"
	"testing"
)

func refsString(refs []FuncRef) string { return fmt.Sprint(refs) }

// Invalid input (security-01 review): a qualified call after a keyword the
// scanner does not list (VARIADIC, ASYMMETRIC, JSON's DEFAULT, XML's
// DOCUMENT) is still a call, so the executor checks it.
func TestIndexFunctionCalls_QualifiedCallAfterUnknownKeyword(t *testing.T) {
	for keys, want := range map[string]string{
		"((concat_ws(',', VARIADIC public.evil(a))))":            "[concat_ws public.evil]",
		"((a BETWEEN ASYMMETRIC public.evil(a) AND b))":          "[public.evil]",
		"((json_value(j, '$' DEFAULT public.evil(b) ON ERROR)))": "[json_value public.evil]",
		"((xmlparse(DOCUMENT public.evil(b))))":                  "[xmlparse public.evil]",
		"((concat_ws(',', VARIADIC evil(a))))":                   "[concat_ws evil]",
	} {
		refs, err := IndexFunctionCalls("CREATE INDEX i ON public.t " + keys)
		if err != nil || refsString(refs) != want {
			t.Errorf("%s: refs=%v err=%v, want %s", keys, refs, err, want)
		}
	}
}

// Boundary: a top-level key element's operator class with parameters is
// not a call, qualified or bare; inside parentheses or in the predicate a
// qualified name before "(" is.
func TestIndexFunctionCalls_OpclassParamsAreNotCalls(t *testing.T) {
	for keys, want := range map[string]string{
		"USING gist (a public.gist_trgm_ops(siglen=32))":        "[]",
		"USING gist (lower(a) gist_trgm_ops(siglen='32'), b)":   "[lower]",
		"USING gist ((a) public.gist_trgm_ops(siglen=32) DESC)": "[]",
		"(a) WHERE b IS NOT NULL AND c public.evil(d)":          "[public.evil]",
		"((a public.evil(d)))":                                  "[public.evil]",
	} {
		refs, err := IndexFunctionCalls("CREATE INDEX i ON public.t " + keys)
		if err != nil || refsString(refs) != want {
			t.Errorf("%s: refs=%v err=%v, want %s", keys, refs, err, want)
		}
	}
}

// Happy path and nil/empty: qualified types (casts, CAST AS, typed
// literals) and OPERATOR(s.op) outside pg_catalog are listed; bare ones,
// pg_catalog's and an index with no expressions list nothing.
func TestIndexObjectRefs(t *testing.T) {
	for keys, want := range map[string]string{
		"((a::public.evil))":                                  "[type public.evil]",
		"((CAST(a AS ext.citext)))":                           "[type ext.citext]",
		"((a::\"My S\".\"T\"[]))":                             "[type My S.T]",
		"((a::public.vector(3)))":                             "[type public.vector]",
		"(a) WHERE a <> public.dom 'x'":                       "[type public.dom]",
		"(a) WHERE a OPERATOR(public.===) 'x'":                "[operator public.===]",
		"(a) WHERE a OPERATOR( ext . -> ) 'x'":                "[operator ext.->]",
		"(a) WHERE a OPERATOR(pg_catalog.=) 1":                "[]",
		"(a) WHERE a OPERATOR(=) 1":                           "[]",
		"((a::text), (b::pg_catalog.int8), (c::varchar(10)))": "[]",
		"((d::timestamp with time zone))":                     "[]",
		"(a) WHERE date '2020-01-01' < d":                     "[]",
		"(a, b)":                                              "[]",
	} {
		refs, err := IndexObjectRefs("CREATE INDEX i ON public.t " + keys)
		if err != nil || fmt.Sprint(refs) != want {
			t.Errorf("%s: refs=%v err=%v, want %s", keys, refs, err, want)
		}
	}
}

// Invalid input: an OPERATOR() form the scanner cannot read is an error,
// never a guess, and so is a statement that is not a CREATE INDEX.
func TestIndexObjectRefs_Unreadable(t *testing.T) {
	for _, keys := range []string{
		"(a) WHERE a OPERATOR(public) 1",
		"(a) WHERE a OPERATOR(public.) 1",
		"(a) WHERE a OPERATOR(public.= 1",
		"(a) WHERE a OPERATOR(public.\"x\") 1",
	} {
		refs, err := IndexObjectRefs("CREATE INDEX i ON public.t " + keys)
		if !errors.Is(err, ErrUnreadableExpression) || refs != nil {
			t.Errorf("%s: refs=%v err=%v, want ErrUnreadableExpression", keys, refs, err)
		}
	}
	if refs, err := IndexObjectRefs("DROP INDEX public.i"); err == nil || refs != nil {
		t.Errorf("DROP INDEX: refs=%v err=%v, want an error", refs, err)
	}
}

// No concurrent access test: IndexObjectRefs is a pure function of its
// argument and shares no state.
