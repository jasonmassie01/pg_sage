package optimizer

import (
	"reflect"
	"strings"
	"testing"
)

// indexes-01: the coverage and duplicate rule compared index keys after
// stripping the operator class, the collation and the sort order, so two
// indexes that serve different operators counted as one: a pattern-ops
// index was a "duplicate" of a plain btree, and tuning could drop an HNSW
// cosine index as "covered" by an l2 one.
//
// No concurrent access tests: the shape rule is pure parsing of its
// arguments and shares no state.

func ix(keys string) string {
	return "CREATE INDEX existing ON public.items USING " + keys
}

// probePairs are the five pairs the feature report probed: each returned
// CoveredBy = true and Subsumes = true in both directions (CHECK-01).
var probePairs = []struct{ name, a, b string }{
	{"hnsw l2 vs cosine", ix("hnsw (embedding vector_l2_ops)"),
		ix("hnsw (embedding vector_cosine_ops)")},
	{"gin jsonb_ops vs jsonb_path_ops", ix("gin (doc)"), ix("gin (doc jsonb_path_ops)")},
	{"btree pattern ops vs plain", ix("btree (email text_pattern_ops)"), ix("btree (email)")},
	{"btree mixed order vs plain", ix("btree (a, b DESC)"), ix("btree (a, b)")},
	{"btree collation vs plain", ix(`btree (name COLLATE "C")`), ix("btree (name)")},
}

func TestCoveredBy_ProbePairsAreDistinct(t *testing.T) {
	for _, p := range probePairs {
		for _, dir := range [][2]string{{p.a, p.b}, {p.b, p.a}} {
			if CoveredBy(dir[0], dir[1]) {
				t.Errorf("%s: CoveredBy(%q, %q) = true", p.name, dir[0], dir[1])
			}
			if Subsumes(dir[1], dir[0]) {
				t.Errorf("%s: Subsumes(%q, %q) = true", p.name, dir[1], dir[0])
			}
		}
		// Control: each side still covers itself, so the pairs fail only
		// on the decoration.
		if !CoveredBy(p.a, p.a) || !CoveredBy(p.b, p.b) {
			t.Errorf("%s: an index no longer covers itself", p.name)
		}
	}
}

var decoratedCoverCases = []struct {
	name, candidate, existing string
	want                      bool
}{
	{"prefix", "btree (a)", "btree (a, b)", true},
	{"single column reversed", "btree (a DESC)", "btree (a)", true},
	{"reversed prefix", "btree (a DESC)", "btree (a, b)", true},
	{"fully reversed", "btree (a DESC, b DESC)", "btree (a, b)", true},
	{"fully reversed, existing side", "btree (a, b DESC)", "btree (a DESC, b)", true},
	{"explicit default order", "btree (a ASC NULLS LAST)", "btree (a)", true},
	{"reversed with nulls", "btree (a NULLS FIRST)", "btree (a DESC NULLS LAST)", true},
	{"mixed order", "btree (a, b DESC)", "btree (a, b)", false},
	{"mixed order, nulls kept", "btree (a, b DESC NULLS LAST)", "btree (a, b)", false},
	{"reversed without its nulls", "btree (a DESC NULLS LAST)", "btree (a)", false},
	{"order beyond the prefix is free", "btree (a)", "btree (a, b DESC)", true},
	{"pattern ops prefix", "btree (email text_pattern_ops)",
		"btree (email text_pattern_ops, id)", true},
	{"pattern ops is not plain", "btree (email)", "btree (email text_pattern_ops, id)", false},
	{"opclass case folds", "btree (email TEXT_PATTERN_OPS)",
		"btree (email text_pattern_ops)", true},
	{"collation prefix", `btree (name COLLATE "C")`, `btree (name COLLATE "C", id)`, true},
	{"pg_catalog collation", `btree (name COLLATE pg_catalog."C")`,
		`btree (name COLLATE "C")`, true},
	{"different collations", `btree (name COLLATE "C")`, `btree (name COLLATE "POSIX")`,
		false},
	{"expression opclass, catalog form", "btree ((lower(email)) text_pattern_ops)",
		"btree (lower((email)::text) text_pattern_ops)", true},
	{"expression opclass vs plain expression", "btree ((lower(email)) text_pattern_ops)",
		"btree (lower((email)::text))", false},
	{"hnsw same opclass", "hnsw (embedding vector_cosine_ops)",
		"hnsw (embedding vector_cosine_ops)", true},
	{"gist opclass parameters equal", "gist (t tsvector_ops(siglen=32))",
		"gist (t tsvector_ops(siglen = 32))", true},
	{"gist opclass parameters differ", "gist (t tsvector_ops(siglen=32))",
		"gist (t tsvector_ops(siglen=64))", false},
	{"gist parameters, catalog quoting", "gist (t tsvector_ops(siglen=32))",
		"gist (t tsvector_ops (siglen='32'))", true},
	{"gist parameters differ, catalog quoting", "gist (t tsvector_ops(siglen=32))",
		"gist (t tsvector_ops (siglen='64'))", false},
	{"gist quoted text parameter kept", "gist (t tsvector_ops(siglen=32))",
		"gist (t tsvector_ops (siglen='3x2'))", false},
	{"gist parameters vs none", "gist (t tsvector_ops(siglen=32))", "gist (t)", false},
	{"include served by a pattern-ops key", "btree (status) INCLUDE (email)",
		"btree (status, email text_pattern_ops)", true},
	{"include, keys differ by opclass", "btree (email) INCLUDE (id)",
		"btree (email text_pattern_ops) INCLUDE (id)", false},
}

func TestCoveredBy_KeysCompareOpclassCollationAndOrder(t *testing.T) {
	for _, c := range decoratedCoverCases {
		got := CoveredBy("CREATE INDEX CONCURRENTLY c ON public.items USING "+c.candidate,
			ix(c.existing))
		if got != c.want {
			t.Errorf("%s: CoveredBy(%s, %s) = %v, want %v", c.name, c.candidate,
				c.existing, got, c.want)
		}
	}
}

// Invalid input: a decoration the scanner cannot read makes the shape
// unparseable, and an unparseable shape covers nothing and is covered by
// nothing.
func TestCoveredBy_MalformedDecorationsCoverNothing(t *testing.T) {
	plain := ix("btree (a)")
	for _, bad := range []string{
		ix("btree (a NULLS SOMETIMES)"),
		ix("btree (a NULLS)"),
		ix("btree (a text_ops text_pattern_ops)"),
		ix("gist (t tsvector_ops(siglen=32)"),
		ix(`btree (a COLLATE)`),
	} {
		if CoveredBy(bad, plain) || CoveredBy(plain, bad) || Subsumes(bad, plain) ||
			Subsumes(plain, bad) {
			t.Errorf("%s: an unparseable shape took part in coverage", bad)
		}
		if _, ok := shapeOf(bad, nil); ok {
			t.Errorf("%s: shapeOf parsed it", bad)
		}
		if _, err := parseIndexKeys(bad); err == nil {
			t.Errorf("%s: parseIndexKeys parsed it", bad)
		}
	}
	for _, empty := range []string{"", "   "} {
		if CoveredBy(empty, plain) || CoveredBy(plain, empty) {
			t.Errorf("%q took part in coverage", empty)
		}
	}
}

func TestParseIndexKeys_OpclassParameters(t *testing.T) {
	got, err := parseIndexKeys(ix("gist (t tsvector_ops(siglen=32))"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []indexKey{{column: "t", decor: keyDecor{opclass: "tsvector_ops(siglen=32)"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %+v, want %+v", got, want)
	}
	// extractColumnsFromDDL still returns plain columns.
	cols := extractColumnsFromDDL(ix(`btree (email text_pattern_ops DESC, name COLLATE "C")`))
	if !reflect.DeepEqual(cols, []string{"email", "name"}) {
		t.Fatalf("columns = %v", cols)
	}
}

// LeadingKey stays the bare first key: the in-flight lock groups by column.
func TestLeadingKey_IgnoresDecorations(t *testing.T) {
	k, ok := LeadingKey(ix(`btree (Email text_pattern_ops DESC NULLS LAST, x)`))
	if !ok || k != "email" {
		t.Fatalf("leading key = %q %v, want email", k, ok)
	}
	k, ok = LeadingKey(ix(`btree (name COLLATE "C")`))
	if !ok || k != "name" {
		t.Fatalf("leading key = %q %v, want name", k, ok)
	}
}

func TestSubsumes_RefusesToReplaceAnOpclassIndex(t *testing.T) {
	wider := "CREATE INDEX CONCURRENTLY w ON public.items (email, created_at)"
	for _, old := range []string{
		ix("btree (email text_pattern_ops)"),
		ix(`btree (email COLLATE "C")`),
		ix("btree (email DESC NULLS LAST)"),
	} {
		if Subsumes(wider, old) {
			t.Errorf("%s replaced %s", wider, old)
		}
	}
	if !Subsumes(wider, ix("btree (email DESC)")) {
		t.Error("a reversed single-column index is served by a backward scan")
	}
}

func opclassContext(defaults map[string]KeyDefault, defs ...string) TableContext {
	tc := exprTableContext()
	tc.KeyDefaults = defaults
	for _, d := range defs {
		tc.Indexes = append(tc.Indexes, validIndex(d))
	}
	return tc
}

// emailDefaults is what PostgreSQL applies to public.orders.email (text,
// database collation) when the DDL names neither.
func emailDefaults() map[string]KeyDefault {
	return map[string]KeyDefault{"email": {Collation: []string{`"default"`},
		Opclass: map[string][]string{"btree": {"text_ops"}, "hash": {"text_ops"}}}}
}

func TestCheckDuplicate_DecoratedKeys(t *testing.T) {
	v := newTestValidator(nil)
	plain := "CREATE INDEX existing ON public.orders USING btree (email)"
	for _, c := range []struct {
		name, ddl string
		defaults  map[string]KeyDefault
		reason    string // "" admitted
	}{
		{"pattern ops beside plain", "(email text_pattern_ops)", emailDefaults(), ""},
		{"collation C beside plain", `(email COLLATE "C")`, emailDefaults(), ""},
		{"explicit default opclass", "(email text_ops)", emailDefaults(), "duplicate"},
		{"explicit default collation", `(email COLLATE "default")`, emailDefaults(),
			"duplicate"},
		{"qualified default collation", `(email COLLATE pg_catalog."default")`,
			emailDefaults(), "duplicate"},
		{"both defaults spelled out", `(email COLLATE "default" text_ops DESC)`,
			emailDefaults(), "duplicate"},
		{"no catalog: an explicit name stays distinct", "(email text_ops)", nil, ""},
		{"reversed single column", "(email DESC)", nil, "duplicate"},
		{"mixed order", "(email, status DESC)", nil, ""},
	} {
		tc := opclassContext(c.defaults, plain)
		ok, reason := v.checkDuplicate(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY c ON public.orders " + c.ddl}, tc)
		switch {
		case c.reason == "" && !ok:
			t.Errorf("%s: rejected: %s", c.name, reason)
		case c.reason != "" && (ok || !strings.Contains(reason, c.reason)):
			t.Errorf("%s: ok=%v reason %q, want %q", c.name, ok, reason, c.reason)
		}
	}
}

// Boundary: a default is per access method, and the existing side is
// normalized the same way as the candidate.
func TestCheckDuplicate_DefaultsArePerMethodAndBothSides(t *testing.T) {
	v := newTestValidator(nil)
	gin := map[string]KeyDefault{"payload": {Opclass: map[string][]string{
		"gin": {"jsonb_ops"}, "btree": {"jsonb_ops"}}}}
	cases := []struct {
		name, ddl, existing string
		defaults            map[string]KeyDefault
		dup                 bool
	}{
		{"hash has no btree default", "USING hash (email text_ops)",
			"CREATE INDEX existing ON public.orders USING hash (email)",
			map[string]KeyDefault{"email": {Opclass: map[string][]string{
				"btree": {"text_ops"}}}}, false},
		{"existing spells the default", "(email)",
			"CREATE INDEX existing ON public.orders USING btree (email text_ops)",
			emailDefaults(), true},
		{"jsonb_ops is the gin default", "USING gin (payload jsonb_ops)",
			"CREATE INDEX existing ON public.orders USING gin (payload)", gin, true},
		{"jsonb_path_ops is not", "USING gin (payload jsonb_path_ops)",
			"CREATE INDEX existing ON public.orders USING gin (payload)", gin, false},
		{"jsonb_ops does not cover jsonb_path_ops", "USING gin (payload)",
			"CREATE INDEX existing ON public.orders USING gin (payload jsonb_path_ops)",
			gin, false},
		{"other column's default does not apply", "(status text_ops)",
			"CREATE INDEX existing ON public.orders USING btree (status)",
			emailDefaults(), false},
	}
	for _, c := range cases {
		tc := opclassContext(c.defaults, c.existing)
		ok, reason := v.checkDuplicate(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY c ON public.orders " + c.ddl}, tc)
		if ok == c.dup {
			t.Errorf("%s: ok=%v (%s), want duplicate=%v", c.name, ok, reason, c.dup)
		}
	}
}

func TestResolveKeyDefaults(t *testing.T) {
	rows := []keyDefaultRow{
		{column: "email", collation: "default", collationSchema: "pg_catalog",
			method: "btree", opclass: "text_ops", opclassSchema: "pg_catalog", exact: true},
		{column: "email", collation: "default", collationSchema: "pg_catalog",
			method: "hash", opclass: "text_ops", opclassSchema: "pg_catalog", exact: true},
		// varchar has no btree opclass of its own: text_ops is binary compatible.
		{column: "vc", collation: "C", collationSchema: "pg_catalog", method: "btree",
			opclass: "text_ops", opclassSchema: "pg_catalog"},
		// varchar is also binary compatible with bpchar: text, the preferred
		// type of the string category, wins.
		{column: "vc2", method: "btree", opclass: "bpchar_ops", opclassSchema: "pg_catalog"},
		{column: "vc2", method: "btree", opclass: "text_ops", opclassSchema: "pg_catalog",
			preferred: true},
		// An exact match wins over a binary-compatible one.
		{column: "t", method: "btree", opclass: "other_ops", opclassSchema: "public"},
		{column: "t", method: "btree", opclass: "t_ops", opclassSchema: "public", exact: true},
		// Two binary-compatible defaults are ambiguous: PostgreSQL would
		// not pick one silently, so neither is taken as the default.
		{column: "amb", method: "btree", opclass: "a_ops", opclassSchema: "public"},
		{column: "amb", method: "btree", opclass: "b_ops", opclassSchema: "public"},
		{column: "id", method: "btree", opclass: "int4_ops", opclassSchema: "pg_catalog",
			exact: true},
		{column: "Odd Col", collation: "my coll", collationSchema: "app"},
	}
	got := resolveKeyDefaults(rows)
	want := map[string]KeyDefault{
		"email": {Collation: []string{`"default"`}, Opclass: map[string][]string{
			"btree": {"text_ops"}, "hash": {"text_ops"}}},
		"vc": {Collation: []string{`"C"`}, Opclass: map[string][]string{
			"btree": {"text_ops"}}},
		"vc2": {Opclass: map[string][]string{"btree": {"text_ops"}}},
		"t":   {Opclass: map[string][]string{"btree": {"t_ops", "public.t_ops"}}},
		"amb": {Opclass: map[string][]string{}},
		"id":  {Opclass: map[string][]string{"btree": {"int4_ops"}}},
		"Odd Col": {Collation: []string{`"my coll"`, `app."my coll"`},
			Opclass: map[string][]string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults:\n got %#v\nwant %#v", got, want)
	}
	if got := resolveKeyDefaults(nil); len(got) != 0 {
		t.Fatalf("no rows = %v", got)
	}
}
