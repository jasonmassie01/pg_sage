package optimizer

import (
	"strings"
	"testing"
)

// No concurrent access tests: binding and qualification are pure functions
// of their arguments.

// bindFunction's table: which schema a call binds to, and why one is
// refused. found rows are sorted by schema, as the catalog query returns.
func TestBindFunction(t *testing.T) {
	cat := schemaFunctions{schema: "pg_catalog", immutable: true, trusted: true}
	ext := schemaFunctions{schema: "ext", immutable: true, trusted: true}
	ext2 := schemaFunctions{schema: "ext2", immutable: true, trusted: true}
	planted := schemaFunctions{schema: "public", immutable: true, trusted: false}
	stable := schemaFunctions{schema: "pg_catalog", immutable: false, trusted: true}
	for _, tc := range []struct {
		name, schema, want, reason string
		fn                         funcCall
		found                      []schemaFunctions
	}{
		{name: "bare binds pg_catalog over a planted overload", fn: funcCall{name: "lower"},
			found: []schemaFunctions{cat, planted}, want: "pg_catalog"},
		{name: "bare binds pg_catalog over an extension", fn: funcCall{name: "f"},
			found: []schemaFunctions{ext, cat}, want: "pg_catalog"},
		{name: "bare binds the one extension", fn: funcCall{name: "soundex"},
			found: []schemaFunctions{ext, planted}, want: "ext"},
		{name: "bare in two extensions is ambiguous", fn: funcCall{name: "f"},
			found:  []schemaFunctions{ext, ext2},
			reason: "function_schema_not_allowed: f is defined by several allowlisted"},
		{name: "bare only planted", fn: funcCall{name: "evil"},
			found: []schemaFunctions{planted}, reason: "function_schema_not_allowed: evil has no"},
		{name: "bare nowhere", fn: funcCall{name: "nope"},
			reason: "function_schema_not_allowed: nope has no"},
		{name: "qualified planted schema", fn: funcCall{schema: "public", name: "lower"},
			found:  []schemaFunctions{planted},
			reason: "function_schema_not_allowed: public.lower resolves to public"},
		{name: "qualified extension", fn: funcCall{schema: "ext", name: "soundex"},
			found: []schemaFunctions{ext}, want: "ext"},
		{name: "qualified schema without it", fn: funcCall{schema: "ext", name: "soundex"},
			reason: "function_schema_not_allowed: ext.soundex has no"},
		{name: "mixed volatility refused", fn: funcCall{name: "date_trunc"},
			found: []schemaFunctions{stable}, reason: "function date_trunc is not IMMUTABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := bindFunction(tc.fn, tc.found)
			if tc.reason != "" {
				if got != "" || !strings.HasPrefix(reason, tc.reason) {
					t.Fatalf("got %q reason %q, want refusal %q", got, reason, tc.reason)
				}
				return
			}
			if got != tc.want || reason != "" {
				t.Fatalf("got %q reason %q, want %q", got, reason, tc.want)
			}
		})
	}
}

// Happy path: every bare call is named by its bound schema, qualified and
// grammar calls are kept, and nothing else changes.
func TestQualifyBoundCalls(t *testing.T) {
	ddl := `CREATE INDEX CONCURRENTLY i ON public.t ((lower(email)), ` +
		`(SOUNDEX (name)), (pg_catalog.upper(a)), (extract(year from d))) ` +
		`WHERE lower(code) <> 'lower(x)'`
	bound := map[funcCall]string{{name: "lower"}: "pg_catalog", {name: "soundex"}: "Ext",
		{schema: "pg_catalog", name: "upper"}: "pg_catalog", {name: "extract"}: "pg_catalog"}
	got, reason := qualifyBoundCalls(ddl, bound)
	want := `CREATE INDEX CONCURRENTLY i ON public.t ((pg_catalog.lower(email)), ` +
		`("Ext".SOUNDEX (name)), (pg_catalog.upper(a)), (extract(year from d))) ` +
		`WHERE pg_catalog.lower(code) <> 'lower(x)'`
	if reason != "" || got != want {
		t.Fatalf("got %q reason %q\nwant %q", got, reason, want)
	}
}

// Nil/empty: no bound calls leaves the DDL byte for byte.
func TestQualifyBoundCalls_NothingBound(t *testing.T) {
	ddl := "CREATE INDEX i ON public.t (a)  ;"
	if got, reason := qualifyBoundCalls(ddl, nil); got != ddl || reason != "" {
		t.Fatalf("got %q reason %q", got, reason)
	}
}

// Error propagation: a bare call the binding does not cover, and an
// expression the scanner cannot read, refuse instead of passing bare.
func TestQualifyBoundCalls_UncoveredCallRefused(t *testing.T) {
	_, reason := qualifyBoundCalls("CREATE INDEX i ON public.t ((lower(a)), (evil(b)))",
		map[funcCall]string{{name: "lower"}: "pg_catalog"})
	if !strings.Contains(reason, "function_schema_not_allowed: the call to evil") {
		t.Fatalf("reason %q", reason)
	}
	_, reason = qualifyBoundCalls(`CREATE INDEX i ON public.t ((lower(a || 'x\y')))`,
		map[funcCall]string{{name: "lower"}: "pg_catalog"})
	if !strings.Contains(reason, "cannot be verified") || !strings.Contains(reason, "backslash") {
		t.Fatalf("reason %q", reason)
	}
}

func TestQualifierFor(t *testing.T) {
	for schema, want := range map[string]string{
		"pg_catalog": "pg_catalog", "public": "public", "ext_1": "ext_1",
		"Ext": `"Ext"`, "user": `"user"`, `a"b`: `"a""b"`, "1x": `"1x"`,
	} {
		if got := qualifierFor(schema); got != want {
			t.Errorf("qualifierFor(%q) = %q, want %q", schema, got, want)
		}
	}
}

// Boundary: a qualified recommendation keeps the identity, the duplicate
// shape and the rejection-memory shape of its bare form, so findings and
// memory recorded before security-01 still match.
func TestCatalogQualifierKeepsIdentityAndShapes(t *testing.T) {
	bare := "CREATE INDEX CONCURRENTLY i ON public.t ((lower(email))) WHERE upper(s) = 'A'"
	qual := "CREATE INDEX CONCURRENTLY i ON public.t ((pg_catalog.lower(email))) " +
		"WHERE pg_catalog . upper(s) = 'A'"
	a := Recommendation{Table: "public.t", DDL: bare}
	b := Recommendation{Table: "public.t", DDL: qual}
	if a.FindingIdentifier() != b.FindingIdentifier() {
		t.Errorf("identity %q != %q", a.FindingIdentifier(), b.FindingIdentifier())
	}
	sa, okA := shapeOf(bare, nil)
	sb, okB := shapeOf(qual, nil)
	if !okA || !okB || sa != sb {
		t.Errorf("duplicate shape %+v != %+v", sa, sb)
	}
	ca, errA := shapeOfCandidate(bare)
	cb, errB := shapeOfCandidate(qual)
	if errA != nil || errB != nil || ca.hash() != cb.hash() {
		t.Errorf("rejection shape %v != %v (%v, %v)", ca, cb, errA, errB)
	}
	other := Recommendation{Table: "public.t",
		DDL: "CREATE INDEX i ON public.t ((my_pg_catalog.lower(email))) WHERE upper(s) = 'A'"}
	if other.FindingIdentifier() == a.FindingIdentifier() {
		t.Error("a schema ending in pg_catalog lost its qualifier")
	}
}
