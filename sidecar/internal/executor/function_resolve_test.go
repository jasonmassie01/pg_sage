package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeFunctionLookup answers from a table keyed by "schema.name" (bare
// names by ".name") and records each lookup.
type fakeFunctionLookup struct {
	mu      sync.Mutex
	answers map[string]functionCandidates
	err     error
	calls   []string
}

func (f *fakeFunctionLookup) lookup(_ context.Context, schema, name string,
) (functionCandidates, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, schema+"."+name)
	if f.err != nil {
		return functionCandidates{}, f.err
	}
	return f.answers[schema+"."+name], nil
}

func indexSQL(keys string) string {
	return "CREATE INDEX CONCURRENTLY i ON public.t " + keys
}

// Happy path: bare pg_catalog functions, pg_catalog-qualified calls,
// grammar forms and members of an extension in its own schema pass.
func TestCheckIndexFunctions_Allowed(t *testing.T) {
	f := &fakeFunctionLookup{answers: map[string]functionCandidates{
		".lower":      {catalog: true},
		"ext.soundex": {catalog: false, inSchema: 2, members: true},
	}}
	sql := indexSQL("((lower(a)), (LOWER(b)), (pg_catalog.upper(c)), " +
		"(extract(year from d)), (ext.soundex(e)))")
	if err := checkIndexFunctions(context.Background(), f.lookup, sql); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ","); got != ".lower,ext.soundex" {
		t.Fatalf("lookups = %q, want one per distinct non-pg_catalog call", got)
	}
}

// Invalid input: a call to a function pg_catalog lacks, and a qualified
// call into a schema holding a function outside any extension, are refused
// before anything runs, with the call named.
func TestCheckIndexFunctions_Refused(t *testing.T) {
	f := &fakeFunctionLookup{answers: map[string]functionCandidates{
		".evil":        {},
		"public.lower": {catalog: true, inSchema: 1, members: false},
		"ext.missing":  {catalog: false, inSchema: 0, members: false},
	}}
	for keys, want := range map[string]string{
		"((evil(a)))":         "evil is not a pg_catalog function",
		"((public.lower(a)))": "public.lower is not a member of an extension",
		"((ext.missing(a)))":  "ext.missing is not a member of an extension",
	} {
		err := checkIndexFunctions(context.Background(), f.lookup, indexSQL(keys))
		if !errors.Is(err, ErrFunctionSchemaNotAllowed) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want ErrFunctionSchemaNotAllowed naming %q", keys, err, want)
		}
	}
}

// Invalid input: an expression the scanner cannot read is refused, never
// passed through unverified.
func TestCheckIndexFunctions_UnreadableRefused(t *testing.T) {
	f := &fakeFunctionLookup{}
	for _, keys := range []string{"((lower/**/(a)))", "((a || $$x$$))", "((lower(a)"} {
		err := checkIndexFunctions(context.Background(), f.lookup, indexSQL(keys))
		if !errors.Is(err, ErrFunctionSchemaNotAllowed) {
			t.Errorf("%s: err = %v, want ErrFunctionSchemaNotAllowed", keys, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("lookups = %v, want none", f.calls)
	}
}

// Nil/empty: other statements and an index without calls need no lookup.
func TestCheckIndexFunctions_NothingToCheck(t *testing.T) {
	f := &fakeFunctionLookup{err: errors.New("must not be called")}
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY public.i",
		"VACUUM (ANALYZE) public.t",
		indexSQL("(a, b) WHERE c IS NOT NULL AND d like ('x%')"),
		"",
	} {
		if err := checkIndexFunctions(context.Background(), f.lookup, sql); err != nil {
			t.Errorf("%q: %v", sql, err)
		}
	}
}

// Error propagation: a failed lookup is returned with the call named, and
// is not mistaken for a refusal.
func TestCheckIndexFunctions_LookupError(t *testing.T) {
	f := &fakeFunctionLookup{err: errors.New("connection refused")}
	err := checkIndexFunctions(context.Background(), f.lookup, indexSQL("((lower(a)))"))
	if err == nil || errors.Is(err, ErrFunctionSchemaNotAllowed) ||
		!strings.Contains(err.Error(), "lower") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the lookup error naming lower", err)
	}
}

// State transitions: every statement of a transaction is checked; one
// refused statement refuses the whole batch.
func TestCheckStatementsFunctions(t *testing.T) {
	f := &fakeFunctionLookup{answers: map[string]functionCandidates{".lower": {catalog: true}}}
	ok := []string{"SET LOCAL lock_timeout = '5s'", indexSQL("((lower(a)))")}
	if err := checkStatementsFunctions(context.Background(), f.lookup, ok); err != nil {
		t.Fatal(err)
	}
	bad := append(ok, indexSQL("((evil(a)))"))
	if err := checkStatementsFunctions(context.Background(), f.lookup, bad); !errors.Is(err,
		ErrFunctionSchemaNotAllowed) {
		t.Fatalf("err = %v, want refused", err)
	}
}

// Concurrent access: the check keeps no state between calls.
func TestCheckIndexFunctions_Concurrent(t *testing.T) {
	f := &fakeFunctionLookup{answers: map[string]functionCandidates{".lower": {catalog: true}}}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys := "((lower(a)))"
			if i%2 == 1 {
				keys = "((evil(a)))"
			}
			err := checkIndexFunctions(context.Background(), f.lookup, indexSQL(keys))
			if (i%2 == 1) != errors.Is(err, ErrFunctionSchemaNotAllowed) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("wrong outcome: %v", err)
	}
}

// Invalid input (static, no database): an unqualified CREATE INDEX target
// and a call qualified by a protected schema are refused by
// ValidateExecutorSQL with distinguishable reasons.
func TestValidateExecutorSQL_IndexTargetsAndFunctionSchemas(t *testing.T) {
	for sql, want := range map[string]string{
		"CREATE INDEX CONCURRENTLY i ON t (a)":      "INDEX target must be schema-qualified",
		"CREATE UNIQUE INDEX i ON t (a)":            "UNIQUE INDEX target must be",
		indexSQL("((sage.f(a)))"):                   `sage.f is in protected schema "sage"`,
		indexSQL("((information_schema._pg_x(a)))"): "protected schema",
		indexSQL("(a) WHERE google_ml.predict(a)"):  "protected schema",
	} {
		err := ValidateExecutorSQL(sql)
		if !errors.Is(err, ErrDisallowedSQL) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", sql, err, want)
		}
	}
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY i ON public.t (a)",
		"CREATE UNIQUE INDEX i ON \"S\".t (a)",
		indexSQL("((pg_catalog.lower(a)), (lower(b)), (public.soundex(c)))"),
	} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
}
