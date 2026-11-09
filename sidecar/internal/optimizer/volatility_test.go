package optimizer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// failingCatalog is a catalog connection whose every read fails; it counts
// the bounded transactions the volatility check opens (one per lookup).
type failingCatalog struct {
	err   error
	begun atomic.Int64
}

func (f *failingCatalog) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	f.begun.Add(1)
	return nil, f.err
}

func failingValidator(err error) (*Validator, *failingCatalog) {
	fc := &failingCatalog{err: err}
	v := NewValidator(nil, fnTestOptimizerConfig(), noopLog)
	v.catalog = fc
	return v, fc
}

// Error propagation: a catalog read error refuses the candidate and the
// reason carries the function and the cause.
func TestVolatilityUnit_ReadErrorRefuses(t *testing.T) {
	v, fc := failingValidator(errors.New("connect: connection refused"))
	ok, reason := v.checkExpressionVolatility(context.Background(),
		Recommendation{DDL: "CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))"})
	if ok {
		t.Fatal("a failed catalog read admitted the candidate")
	}
	for _, want := range []string{"volatility lookup", "lower", "connection refused"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
	if got := fc.begun.Load(); got != 1 {
		t.Errorf("lookups = %d, want 1 (refused at the first error)", got)
	}
}

// Nil/empty: an index with no expressions (or a DDL that does not parse)
// issues no lookup.
func TestVolatilityUnit_NoExpressionNoLookup(t *testing.T) {
	v, fc := failingValidator(errors.New("must not be called"))
	for _, ddl := range []string{
		"CREATE INDEX CONCURRENTLY i ON public.t (a, b)",
		"CREATE INDEX CONCURRENTLY i ON public.t (a) INCLUDE (b)",
		"CREATE INDEX CONCURRENTLY i ON public.t (a) WHERE b IS NOT NULL",
		"",
		"no parens at all",
	} {
		ok, reason := v.checkExpressionVolatility(context.Background(),
			Recommendation{DDL: ddl})
		if !ok || reason != "" {
			t.Errorf("%q: ok=%t reason=%q, want admitted", ddl, ok, reason)
		}
	}
	if got := fc.begun.Load(); got != 0 {
		t.Errorf("lookups = %d, want 0", got)
	}
}

// Keywords that take parentheses are grammar, never looked up; a
// function repeated in keys and predicate is looked up once.
func TestVolatilityUnit_KeywordsSkippedAndNamesDeduplicated(t *testing.T) {
	v, fc := failingValidator(errors.New("must not be called"))
	ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY i ON public.t ((coalesce(a, b))) " +
			"WHERE a in (1, 2) AND (b > 0) or not (c) and exists (d)"})
	if !ok || reason != "" || fc.begun.Load() != 0 {
		t.Fatalf("ok=%t reason=%q lookups=%d, want admitted with no lookup",
			ok, reason, fc.begun.Load())
	}
	ok, _ = v.checkExpressionVolatility(context.Background(), Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY i ON public.t ((lower(a)), (LOWER(b))) " +
			"WHERE lower(c) <> ''"})
	if ok || fc.begun.Load() != 1 {
		t.Fatalf("ok=%t lookups=%d, want refused after exactly one lookup",
			ok, fc.begun.Load())
	}
}

// The shipped allowlist names the extensions whose IMMUTABLE functions
// are used in expression indexes, and nothing that is not an extension.
func TestVolatilityUnit_DefaultAllowlist(t *testing.T) {
	v := NewValidator(nil, fnTestOptimizerConfig(), noopLog)
	if !slices.Equal(v.extensions, expressionIndexExtensions) {
		t.Fatalf("validator allowlist = %v, want %v", v.extensions, expressionIndexExtensions)
	}
	for _, want := range []string{"fuzzystrmatch", "pg_trgm", "postgis", "citext"} {
		if !slices.Contains(v.extensions, want) {
			t.Errorf("allowlist %v lacks %s", v.extensions, want)
		}
	}
	for _, never := range []string{"", "seg", "pgcrypto", "plpgsql", "public", "pg_catalog"} {
		if slices.Contains(v.extensions, never) {
			t.Errorf("allowlist %v must not contain %q", v.extensions, never)
		}
	}
	if v.catalog != nil {
		t.Error("a validator without a pool has a catalog connection")
	}
}

// Concurrent access: the check keeps no state between calls, and parallel
// callers each get their own refusal and their own lookup.
func TestVolatilityUnit_ConcurrentCallsIndependent(t *testing.T) {
	v, fc := failingValidator(errors.New("connection reset"))
	const n = 16
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
				DDL: "CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)))"})
			if ok || !strings.Contains(reason, "connection reset") {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 0 || fc.begun.Load() != n {
		t.Fatalf("wrong outcomes=%d lookups=%d, want 0 and %d",
			admitted.Load(), fc.begun.Load(), n)
	}
}

// Boundary (real PG): with fuzzystrmatch taken off the allowlist, its own
// soundex() is refused even though the extension is installed.
func TestVolatility_ExtensionOffAllowlistRefused(t *testing.T) {
	v, pool := volatilityValidator(t)
	extensionSchema(t, pool, "fuzzystrmatch")
	ddl := "CREATE INDEX CONCURRENTLY i ON public.t ((soundex(name)))"
	ok, reason := checkVol(v, context.Background(), ddl)
	wantAdmitted(t, ok, reason)
	v.extensions = []string{"pg_trgm"}
	ok, reason = checkVol(v, context.Background(), ddl)
	wantRefused(t, ok, reason, "function_schema_not_allowed", "soundex")
	v.extensions = nil
	ok, reason = checkVol(v, context.Background(), ddl)
	wantRefused(t, ok, reason, "function_schema_not_allowed", "soundex")
}

// Timeouts: the optimizer's catalog-read bounds reach the validator.
func TestVolatilityUnit_OptimizerPassesCatalogTimeouts(t *testing.T) {
	custom := catalogread.Timeouts{
		Statement: 1234 * time.Millisecond, Lock: 567 * time.Millisecond}
	o := New(nil, fnTestOptimizerConfig(), noopLog, WithCatalogReadTimeouts(custom))
	if o.validator.timeouts != custom {
		t.Fatalf("validator timeouts = %+v, want %+v", o.validator.timeouts, custom)
	}
}

// Invalid input (review): a quoted function name or schema, or a comment
// between a name and its "(", hides the call from name extraction, so the
// index is refused before any lookup. The literal case shows why quoted
// names are refused rather than parsed: '"' || evil(x) || '"(' would read
// as one quoted name "' || evil(x) || '" and hide evil().
func TestVolatilityUnit_UnverifiableCallRefused(t *testing.T) {
	v, fc := failingValidator(errors.New("must not be called"))
	for _, keys := range []string{
		`("lower"(email))`,
		`("public".lower(email))`,
		`(public."lower"(email))`,
		`(lower/**/(email))`,
		"(evil -- hidden\n(email))",
		`(('a"' || evil(x) || '"('))`,
	} {
		ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY i ON public.t (" + keys + ")"})
		if ok || !strings.HasPrefix(reason, "function_schema_not_allowed") {
			t.Errorf("%s: ok=%t reason=%q, want function_schema_not_allowed", keys, ok, reason)
		}
	}
	if got := fc.begun.Load(); got != 0 {
		t.Errorf("lookups = %d, want 0", got)
	}
}

// Invalid input (review): a schema-qualified call and a non-ASCII name are
// looked up (and refused here, where every read fails) instead of slipping
// past the ASCII-only, unqualified name match.
func TestVolatilityUnit_QualifiedAndUnicodeNamesLookedUp(t *testing.T) {
	v, fc := failingValidator(errors.New("connection refused"))
	for _, tc := range []struct{ keys, want string }{
		{"((public.lower(email)))", "public.lower"},
		{"((fé(email)))", "fé"},
		{"((a$b(email)))", "a$b"},
	} {
		before := fc.begun.Load()
		ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY i ON public.t " + tc.keys})
		if ok || !strings.Contains(reason, "lookup for "+tc.want+" failed") {
			t.Errorf("%s: ok=%t reason=%q, want a failed lookup of %s",
				tc.keys, ok, reason, tc.want)
		}
		if fc.begun.Load() != before+1 {
			t.Errorf("%s: lookups = %d, want 1", tc.keys, fc.begun.Load()-before)
		}
	}
}

// Boundary (review): TRIM is grammar that PostgreSQL always binds to
// pg_catalog.btrim/ltrim/rtrim (all IMMUTABLE), and FROM/FOR inside
// TRIM/SUBSTRING are keywords; none is looked up.
func TestVolatilityUnit_TrimFromForSkipped(t *testing.T) {
	v, fc := failingValidator(errors.New("must not be called"))
	ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY i ON public.t ((trim(email)), " +
			"(trim(both from (name)))) WHERE (a) is distinct from (b)"})
	if !ok || reason != "" || fc.begun.Load() != 0 {
		t.Fatalf("ok=%t reason=%q lookups=%d, want admitted with no lookup",
			ok, reason, fc.begun.Load())
	}
}

// Invalid input (second review): where the Go name match and the
// PostgreSQL lexer disagree, the call is refused, never admitted.
// TRİM and LIKE (K is the Kelvin sign) fold to trim and like in Go but
// stay distinct names in PostgreSQL; ☃ is an identifier byte to
// PostgreSQL; ilike, similar and is are callable function names; a
// UESCAPE name hides the quote before "("; and PostgreSQL 16+ reads \v as
// whitespace (the E” string leaves the rest unfolded and uncollapsed).
func TestVolatilityUnit_LexerDivergenceRefused(t *testing.T) {
	v, _ := failingValidator(errors.New("connection refused"))
	for _, tc := range []struct{ keys, want string }{
		{"((TRİM(email)))", "function_schema_not_allowed"},
		{"((LI\u212aE(email, 'a')))", "function_schema_not_allowed"},
		{"((sage☃(email)))", "sage☃"},
		{"((★.lower(email)))", "★.lower"},
		{"((similar(email)))", "similar"},
		{"((ilike(email)))", "ilike"},
		{"((is(email)))", "is"},
		{`((U&"l!006fwer" UESCAPE '!'(email)))`, "function_schema_not_allowed"},
		{"((E'\\'' || evil\v(email) || E'\\''))", "evil"},
	} {
		ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY i ON public.t " + tc.keys})
		if ok || !strings.Contains(reason, tc.want) {
			t.Errorf("%q: ok=%t reason=%q, want refused naming %q",
				tc.keys, ok, reason, tc.want)
		}
	}
}

// Happy path (second review): LIKE written with a parenthesised pattern
// is looked up, and pg_catalog.like is IMMUTABLE in every overload, so a
// non-ASCII letter in a literal or an ordinary call does not refuse.
func TestVolatilityUnit_LikeLookedUpNotSkipped(t *testing.T) {
	v, fc := failingValidator(errors.New("connection refused"))
	ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{
		DDL: "CREATE INDEX CONCURRENTLY i ON public.t (email) WHERE email like ('a%')"})
	if ok || !strings.Contains(reason, "lookup for like failed") || fc.begun.Load() != 1 {
		t.Fatalf("ok=%t reason=%q lookups=%d, want one failed lookup of like",
			ok, reason, fc.begun.Load())
	}
}
