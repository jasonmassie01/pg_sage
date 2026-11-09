package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// fakeObjectLookup answers from a table keyed by ref.String() and records
// each lookup.
type fakeObjectLookup struct {
	mu      sync.Mutex
	answers map[string]objectCandidates
	err     error
	calls   []string
}

func (f *fakeObjectLookup) lookup(_ context.Context, ref optimizer.ObjectRef,
) (objectCandidates, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ref.String())
	if f.err != nil {
		return objectCandidates{}, f.err
	}
	return f.answers[ref.String()], nil
}

// Happy path: an extension's type and operator pass, each looked up once;
// bare and pg_catalog types and operators need no lookup.
func TestCheckIndexObjects_ExtensionMembersAllowed(t *testing.T) {
	f := &fakeObjectLookup{answers: map[string]objectCandidates{
		"type ext.citext":   {count: 1, trusted: true},
		"operator ext.=":    {count: 3, trusted: true},
		"type public.never": {},
	}}
	sql := indexSQL("((a::ext.citext), (b::text), (c::pg_catalog.int8)) " +
		"WHERE a::ext.citext OPERATOR(ext.=) 'x' AND b OPERATOR(pg_catalog.=) 'y'")
	if err := checkIndexObjects(context.Background(), f.lookup, sql); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, ","); got != "type ext.citext,operator ext.=" {
		t.Fatalf("lookups = %q, want one per distinct qualified object", got)
	}
}

// Invalid input: a type or operator that does not exist in the schema, or
// is not every one an extension member, is refused with the object named.
func TestCheckIndexObjects_PlantedRefused(t *testing.T) {
	f := &fakeObjectLookup{answers: map[string]objectCandidates{
		"type public.dom":     {count: 1, trusted: false},
		"operator public.===": {count: 1, trusted: false},
		"operator ext.=":      {count: 2, trusted: false},
	}}
	for keys, want := range map[string]string{
		"((a::public.dom))":                      "type public.dom",
		"((CAST(a AS public.missing)))":          "type public.missing",
		"(a) WHERE a <> public.dom 'x'":          "type public.dom",
		"(a) WHERE a OPERATOR(public.===) 'x'":   "operator public.===",
		"(a) WHERE a OPERATOR(ext.=) 'x'":        "operator ext.=",
		"(a) WHERE a OPERATOR(public.\"x\") 'x'": "cannot be verified",
	} {
		err := checkIndexObjects(context.Background(), f.lookup, indexSQL(keys))
		if !errors.Is(err, ErrFunctionSchemaNotAllowed) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want ErrFunctionSchemaNotAllowed naming %q", keys, err, want)
		}
	}
}

// Error propagation: a failed lookup refuses the index, keeps the cause
// and is not mistaken for a refusal by schema.
func TestCheckIndexObjects_LookupError(t *testing.T) {
	cause := errors.New("connection refused")
	f := &fakeObjectLookup{err: cause}
	err := checkIndexObjects(context.Background(), f.lookup, indexSQL("((a::ext.citext))"))
	if !errors.Is(err, cause) || errors.Is(err, ErrFunctionSchemaNotAllowed) ||
		!strings.Contains(err.Error(), "type ext.citext") {
		t.Fatalf("err = %v, want the lookup error naming the type", err)
	}
}

// Nil/empty and state: statements other than CREATE INDEX, and an index
// naming no qualified object, need no lookup; checkStatementsObjects
// stops at the first refusal.
func TestCheckStatementsObjects(t *testing.T) {
	f := &fakeObjectLookup{answers: map[string]objectCandidates{}}
	ok := []string{"DROP INDEX CONCURRENTLY public.i", "ANALYZE public.t", indexSQL("(a, b)")}
	if err := checkStatementsObjects(context.Background(), f.lookup, ok); err != nil ||
		len(f.calls) != 0 {
		t.Fatalf("err = %v, lookups = %v, want none", err, f.calls)
	}
	bad := []string{indexSQL("((a::public.dom))"), indexSQL("((b::public.other))")}
	err := checkStatementsObjects(context.Background(), f.lookup, bad)
	if !errors.Is(err, ErrFunctionSchemaNotAllowed) || len(f.calls) != 1 {
		t.Fatalf("err = %v, lookups = %v, want refused at the first", err, f.calls)
	}
}

// Concurrent access: the check keeps no state of its own, so parallel
// checks through one lookup each see their own answer.
func TestCheckIndexObjects_Concurrent(t *testing.T) {
	f := &fakeObjectLookup{answers: map[string]objectCandidates{
		"type ext.citext": {count: 1, trusted: true},
	}}
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := "((a::ext.citext))"
			if i%2 == 1 {
				keys = "((a::public.dom))"
			}
			errs[i] = checkIndexObjects(context.Background(), f.lookup, indexSQL(keys))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if (i%2 == 0) != (err == nil) {
			t.Errorf("check %d: err = %v", i, err)
		}
	}
}

// Invalid input, statically: a type or operator in a protected schema is
// refused by ValidateExecutorSQL without a catalog read.
func TestValidateExecutorSQL_ProtectedObjectSchemas(t *testing.T) {
	for _, keys := range []string{
		"((a::sage.secret))",
		"(a) WHERE a OPERATOR(google_ml.=) 'x'",
		"(a) WHERE a <> information_schema.cardinal_number '1'",
	} {
		err := ValidateExecutorSQL(indexSQL(keys))
		if !errors.Is(err, ErrDisallowedSQL) || !strings.Contains(err.Error(), "protected schema") {
			t.Errorf("%s: err = %v, want refused as a protected schema", keys, err)
		}
	}
	if err := ValidateExecutorSQL(indexSQL("((a::ext.citext))")); err != nil {
		t.Errorf("extension type refused statically: %v", err)
	}
}
