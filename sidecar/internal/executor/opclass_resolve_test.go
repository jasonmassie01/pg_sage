package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: qualifyIndexOpclasses keeps no state between
// calls; its per-statement cache lives on the stack of one call.

type lookupCall struct{ name, method string }

// fakeOpclassLookup answers from a table keyed by name and access method
// and records every call.
type fakeOpclassLookup struct {
	answers map[lookupCall]opclassCandidates
	err     error
	calls   []lookupCall
}

func (f *fakeOpclassLookup) lookup(
	_ context.Context, name, method string,
) (opclassCandidates, error) {
	f.calls = append(f.calls, lookupCall{name, method})
	if f.err != nil {
		return opclassCandidates{}, f.err
	}
	return f.answers[lookupCall{name, method}], nil
}

func failingLookup(t *testing.T) opclassLookup {
	return func(_ context.Context, name, method string) (opclassCandidates, error) {
		t.Fatalf("unexpected catalog lookup of %s for %s", name, method)
		return opclassCandidates{}, nil
	}
}

// Nil/empty: statements that name no unqualified operator class pass
// through unchanged without a catalog read.
func TestQualifyIndexOpclassesPassesOtherStatementsThrough(t *testing.T) {
	for _, sql := range []string{
		"",
		"DROP INDEX CONCURRENTLY public.i",
		"VACUUM public.t",
		"ALTER TABLE public.t SET (fillfactor = 90)",
		"CREATE STATISTICS public.s ON a, b FROM public.t",
		"CREATE INDEX CONCURRENTLY i ON public.t (a, b DESC)",
		"CREATE INDEX i ON public.t USING gin (a public.gin_trgm_ops)",
		`CREATE INDEX i ON public.t USING gin (a "public"."gin_trgm_ops")`,
		// Unreadable CREATE INDEX: PostgreSQL, not a guess, decides.
		"CREATE INDEX i ON public.t USING gin (a gin_trgm_ops",
	} {
		got, err := qualifyIndexOpclasses(context.Background(), failingLookup(t), sql)
		if err != nil || got != sql {
			t.Errorf("qualify(%q) = %q, %v; want unchanged", sql, got, err)
		}
	}
}

// Happy path: a bare operator class that one installed extension defines
// gets that extension's schema, quoted; the rest of the statement is kept
// byte for byte.
func TestQualifyIndexOpclassesNamesTheExtensionSchema(t *testing.T) {
	f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{
		{"gin_trgm_ops", "gin"}: {extension: []string{"public"}},
	}}
	sql := "CREATE INDEX CONCURRENTLY i ON public.t USING GIN (email gin_trgm_ops) WHERE x"
	got, err := qualifyIndexOpclasses(context.Background(), f.lookup, sql)
	want := `CREATE INDEX CONCURRENTLY i ON public.t USING GIN ` +
		`(email "public".gin_trgm_ops) WHERE x`
	if err != nil || got != want {
		t.Fatalf("qualify = %q, %v; want %q", got, err, want)
	}
	if len(f.calls) != 1 || f.calls[0] != (lookupCall{"gin_trgm_ops", "gin"}) {
		t.Fatalf("lookups = %+v, want one of gin_trgm_ops for gin", f.calls)
	}
}

// Several keys: each is qualified with its own schema (an odd schema name
// is quoted), spliced without disturbing the other spans; a name used
// twice is looked up once.
func TestQualifyIndexOpclassesSeveralKeys(t *testing.T) {
	f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{
		{"gist_trgm_ops", "gist"}:   {extension: []string{"public"}},
		{"gist_hstore_ops", "gist"}: {extension: []string{`ext"s`}},
	}}
	sql := "CREATE INDEX i ON public.t USING gist " +
		"(a gist_trgm_ops, b GIST_HSTORE_OPS, (lower(c)) gist_trgm_ops(siglen=64))"
	got, err := qualifyIndexOpclasses(context.Background(), f.lookup, sql)
	want := `CREATE INDEX i ON public.t USING gist (a "public".gist_trgm_ops, ` +
		`b "ext""s".GIST_HSTORE_OPS, (lower(c)) "public".gist_trgm_ops(siglen=64))`
	if err != nil || got != want {
		t.Fatalf("qualify = %q, %v;\nwant     %q", got, err, want)
	}
	if len(f.calls) != 2 {
		t.Fatalf("lookups = %+v, want one per distinct operator class", f.calls)
	}
}

// Boundary: a pg_catalog operator class of the name wins, exactly as it
// did on the role's default path (pg_catalog is searched first); the bare
// name already resolves to it under the pin. The default method is btree
// and a quoted name keeps its case.
func TestQualifyIndexOpclassesKeepsCatalogOpclasses(t *testing.T) {
	f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{
		{"text_pattern_ops", "btree"}: {catalog: true, extension: []string{"public"},
			other: []string{"app"}},
		{"Mixed_Ops", "btree"}: {catalog: true},
	}}
	sql := `CREATE INDEX i ON public.t (a text_pattern_ops, b "Mixed_Ops")`
	got, err := qualifyIndexOpclasses(context.Background(), f.lookup, sql)
	if err != nil || got != sql {
		t.Fatalf("qualify = %q, %v; want unchanged", got, err)
	}
	want := []lookupCall{{"text_pattern_ops", "btree"}, {"Mixed_Ops", "btree"}}
	if len(f.calls) != 2 || f.calls[0] != want[0] || f.calls[1] != want[1] {
		t.Fatalf("lookups = %+v, want %+v", f.calls, want)
	}
}

// Invalid input: an operator class defined outside every extension, or by
// more than one extension, is refused with ErrUnresolvedOpclass and a
// message that names the schemas, before anything runs.
func TestQualifyIndexOpclassesRefusesUnsafeOrAmbiguousNames(t *testing.T) {
	cases := []struct {
		cands opclassCandidates
		want  []string
	}{
		{opclassCandidates{other: []string{"public"}},
			[]string{"planted_ops", "not part of an extension", "public"}},
		{opclassCandidates{extension: []string{"ext_a", "ext_b"}},
			[]string{"planted_ops", "more than one extension", "ext_a", "ext_b"}},
		{opclassCandidates{extension: []string{"ext_a", "ext_b"}, other: []string{"public"}},
			[]string{"more than one extension"}},
	}
	for _, c := range cases {
		f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{
			{"planted_ops", "btree"}: c.cands}}
		got, err := qualifyIndexOpclasses(context.Background(), f.lookup,
			"CREATE INDEX i ON public.t (a planted_ops)")
		if !errors.Is(err, ErrUnresolvedOpclass) || got != "" {
			t.Errorf("%+v: qualify = %q, %v; want ErrUnresolvedOpclass", c.cands, got, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%+v: error %q does not mention %q", c.cands, err, w)
			}
		}
	}
}

// Nil/empty: an operator class the catalog does not have at all is left
// for PostgreSQL to report (42704); nothing is invented.
func TestQualifyIndexOpclassesLeavesUnknownNames(t *testing.T) {
	f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{}}
	sql := "CREATE INDEX i ON public.t USING gin (a no_such_ops)"
	got, err := qualifyIndexOpclasses(context.Background(), f.lookup, sql)
	if err != nil || got != sql || len(f.calls) != 1 {
		t.Fatalf("qualify = %q, %v (calls %+v); want unchanged after one lookup",
			got, err, f.calls)
	}
}

// Error propagation: a failed catalog read refuses the statement with the
// cause attached and the operator class named.
func TestQualifyIndexOpclassesPropagatesLookupErrors(t *testing.T) {
	cause := errors.New("connection refused")
	f := &fakeOpclassLookup{err: cause}
	got, err := qualifyIndexOpclasses(context.Background(), f.lookup,
		"CREATE INDEX i ON public.t USING gin (a gin_trgm_ops)")
	if !errors.Is(err, cause) || got != "" || !strings.Contains(err.Error(), "gin_trgm_ops") {
		t.Fatalf("qualify = %q, %v; want the lookup error naming gin_trgm_ops", got, err)
	}
	if errors.Is(err, ErrUnresolvedOpclass) {
		t.Fatalf("a failed read is not a resolution verdict: %v", err)
	}
}

// Statements in one transaction: only the CREATE INDEX that names a bare
// extension operator class changes; order and the others are kept.
func TestQualifyStatementsOpclasses(t *testing.T) {
	f := &fakeOpclassLookup{answers: map[lookupCall]opclassCandidates{
		{"gin_trgm_ops", "gin"}: {extension: []string{"public"}},
	}}
	in := []string{
		"ANALYZE public.t",
		"CREATE INDEX i ON public.t USING gin (a gin_trgm_ops)",
		"DROP STATISTICS IF EXISTS public.s",
	}
	got, err := qualifyStatementsOpclasses(context.Background(), f.lookup, in)
	if err != nil || len(got) != 3 || got[0] != in[0] || got[2] != in[2] ||
		got[1] != `CREATE INDEX i ON public.t USING gin (a "public".gin_trgm_ops)` {
		t.Fatalf("qualify statements = %q, %v", got, err)
	}
	if in[1] != "CREATE INDEX i ON public.t USING gin (a gin_trgm_ops)" {
		t.Fatalf("input slice modified: %q", in)
	}
	f.err = errors.New("boom")
	if out, err := qualifyStatementsOpclasses(context.Background(), f.lookup, in); err == nil ||
		out != nil {
		t.Fatalf("lookup failure = %q, %v; want nil and an error", out, err)
	}
}
