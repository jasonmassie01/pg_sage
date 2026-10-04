package facts

import (
	"errors"
	"strings"
	"testing"
)

// Subjects are typed patterns: identifiers fold like PostgreSQL's (unquoted
// lower-case, quoted exact), '*' is the only wildcard, index and table
// subjects must be schema-qualified, and no pattern may reach pg_sage's
// own schema or the system schemas.

func TestParsePatternCanonicalForms(t *testing.T) {
	cases := []struct {
		kind         Kind
		raw, want    string
		schema, name string
	}{
		{KindIndex, "public.idx_orders_status", "public.idx_orders_status",
			"public", "idx_orders_status"},
		{KindIndex, "PUBLIC.IDX_Orders", "public.idx_orders", "public", "idx_orders"},
		{KindTable, `"Billing"."Invoices"`, `"Billing"."Invoices"`, "Billing", "Invoices"},
		{KindTable, `"we""ird".t`, `"we""ird".t`, `we"ird`, "t"},
		{KindTable, "  public . orders  ", "public.orders", "public", "orders"},
		{KindTable, `public."Orders.2024"`, `public."Orders.2024"`, "public", "Orders.2024"},
		{KindIndex, "app.idx_thesis_*", "app.idx_thesis_*", "app", "idx_thesis_*"},
		{KindSchema, "test_*", "test_*", "test_*", ""},
		{KindSchema, "Test_Memory_*", "test_memory_*", "test_memory_*", ""},
		{KindSchema, `"QA Fixtures"`, `"QA Fixtures"`, "QA Fixtures", ""},
		{KindSlot, "debezium_orders", "debezium_orders", "", "debezium_orders"},
		{KindSlot, "airbyte_*", "airbyte_*", "", "airbyte_*"},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.kind, c.raw)
		if err != nil {
			t.Fatalf("%s %q: %v", c.kind, c.raw, err)
		}
		if p.String() != c.want || p.Schema != c.schema || p.Name != c.name ||
			p.Kind != c.kind {
			t.Fatalf("%s %q = %+v (%s), want %s schema=%q name=%q", c.kind, c.raw, p,
				p.String(), c.want, c.schema, c.name)
		}
		again, err := ParsePattern(c.kind, p.String())
		if err != nil || again != p {
			t.Fatalf("canonical %q does not round-trip: %+v %v", p.String(), again, err)
		}
	}
}

func TestParsePatternRejectsMalformedSubjects(t *testing.T) {
	long := strings.Repeat("a", 64)
	cases := []struct {
		kind Kind
		raw  string
	}{
		{KindIndex, ""}, {KindIndex, "   "},
		{KindIndex, "idx_orders"}, // index and table subjects need a schema
		{KindTable, "orders"},
		{KindTable, "public."}, {KindTable, ".orders"}, {KindTable, "a.b.c"},
		{KindTable, `"public.orders`}, {KindTable, `public."orders`},
		{KindTable, "public.orders; DROP TABLE x"}, {KindTable, "public.ord ers"},
		{KindTable, "public.orders--"}, {KindTable, "public.(orders)"},
		{KindTable, "public." + long}, {KindSchema, long},
		{KindSchema, "a.b"}, {KindSchema, `""`},
		{KindSlot, "Debezium"}, {KindSlot, "slot-1"}, {KindSlot, "a.b"}, {KindSlot, ""},
		{KindSlot, strings.Repeat("s", 64)},
		{Kind("view"), "public.v"},
	}
	for _, c := range cases {
		if p, err := ParsePattern(c.kind, c.raw); err == nil {
			t.Fatalf("%s %q accepted as %+v", c.kind, c.raw, p)
		} else if !errors.Is(err, ErrInvalidSubject) && !errors.Is(err, ErrInvalidKind) {
			t.Fatalf("%s %q: %v is not ErrInvalidSubject/ErrInvalidKind", c.kind, c.raw, err)
		}
	}
}

// A subject that could match pg_sage's own schema (or a system schema) is
// refused outright: a fact must never be able to touch the sage schema.
func TestParsePatternRefusesSageAndSystemSchemas(t *testing.T) {
	cases := []struct {
		kind Kind
		raw  string
	}{
		{KindSchema, "sage"}, {KindSchema, "SAGE"}, {KindSchema, `"sage"`},
		{KindSchema, "*"}, {KindSchema, "s*"}, {KindSchema, "*age"}, {KindSchema, "s*e"},
		{KindSchema, "sa*"}, {KindSchema, "pg_catalog"}, {KindSchema, "pg_*"},
		{KindSchema, "information_schema"}, {KindSchema, "pg_toast"}, {KindSchema, "p*"},
		{KindTable, "sage.findings"}, {KindTable, "*.orders"}, {KindIndex, "sage.*"},
		{KindIndex, "s*.idx"}, {KindTable, "pg_catalog.pg_class"},
	}
	for _, c := range cases {
		_, err := ParsePattern(c.kind, c.raw)
		if !errors.Is(err, ErrProtectedSubject) {
			t.Fatalf("%s %q: err = %v, want ErrProtectedSubject", c.kind, c.raw, err)
		}
	}
	// A differently-cased quoted schema is another schema: allowed.
	if _, err := ParsePattern(KindSchema, `"Sage"`); err != nil {
		t.Fatalf(`"Sage" is not pg_sage's schema: %v`, err)
	}
	if _, err := ParsePattern(KindSchema, "sages_test"); err != nil {
		t.Fatalf("sages_test is not sage: %v", err)
	}
}

func TestPatternMatchesTypedObjects(t *testing.T) {
	idx := func(s, n, ts, tn string) ObjectRef {
		return ObjectRef{Kind: KindIndex, Schema: s, Name: n, TableSchema: ts, TableName: tn}
	}
	table := ObjectRef{Kind: KindTable, Schema: "public", Name: "orders"}
	rel := ObjectRef{Kind: KindRelation, Schema: "public", Name: "orders"}
	slot := ObjectRef{Kind: KindSlot, Name: "debezium_orders"}
	cases := []struct {
		kind  Kind
		raw   string
		ref   ObjectRef
		match bool
	}{
		{KindIndex, "public.idx_orders_*", idx("public", "idx_orders_status", "", ""), true},
		{KindIndex, "public.idx_orders_*", idx("app", "idx_orders_status", "", ""), false},
		{KindIndex, "public.idx_orders_*", table, false},
		{KindIndex, "public.orders", rel, true}, // unknown relation kind: narrow
		{KindTable, "public.orders", table, true},
		{KindTable, "public.orders", rel, true},
		{KindTable, "public.orders", idx("public", "i1", "public", "orders"), true},
		{KindTable, "public.orders", idx("public", "i1", "public", "orders_old"), false},
		{KindTable, "public.ord*", table, true},
		{KindTable, "public.orders", ObjectRef{Kind: KindTable, Schema: "", Name: "orders"},
			true}, // unqualified target: any schema could be meant
		{KindTable, `"Public".orders`, table, false},
		{KindSchema, "test_*", ObjectRef{Kind: KindTable, Schema: "test_abc", Name: "t"}, true},
		{KindSchema, "test_*", idx("x", "i", "test_abc", "t"), true},
		{KindSchema, "test_*", ObjectRef{Kind: KindTable, Schema: "app", Name: "test_t"},
			false},
		{KindSchema, "test_*", slot, false},
		{KindSlot, "debezium_*", slot, true},
		{KindSlot, "debezium_*", table, false},
		{KindSlot, "airbyte", slot, false},
	}
	for _, c := range cases {
		p, err := ParsePattern(c.kind, c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Matches(c.ref); got != c.match {
			t.Fatalf("%s %q matches %+v = %v, want %v", c.kind, c.raw, c.ref, got, c.match)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "abc", true}, {"a*", "a", true}, {"a*c", "abbbc", true},
		{"a*c", "abcd", false}, {"*_test", "orders_test", true}, {"*_test", "test", false},
		{"t*_*_x", "t_a_b_x", true}, {"abc", "ABC", false}, {"a**b", "ab", true},
		{"test_%", "test_x", false}, // % and _ are literal, never LIKE wildcards
		{"test__", "test_x", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestParseObjectRefFromTargets(t *testing.T) {
	cases := []struct {
		raw  string
		want ObjectRef
	}{
		{"public.orders", ObjectRef{Kind: KindRelation, Schema: "public", Name: "orders"}},
		{`"Billing"."Invoices"`, ObjectRef{Kind: KindRelation, Schema: "Billing",
			Name: "Invoices"}},
		{"public.orders|btree(status)", ObjectRef{Kind: KindRelation, Schema: "public",
			Name: "orders"}},
		{"orders", ObjectRef{Kind: KindRelation, Name: "orders"}},
		{"slot:debezium_orders", ObjectRef{Kind: KindSlot, Name: "debezium_orders"}},
	}
	for _, c := range cases {
		got, err := ParseObjectRef(c.raw)
		if err != nil || got != c.want {
			t.Fatalf("ParseObjectRef(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
	}
	for _, raw := range []string{"", "pid:123", "public.", `"open`, "slot:"} {
		if got, err := ParseObjectRef(raw); err == nil {
			t.Fatalf("ParseObjectRef(%q) = %+v, want error", raw, got)
		}
	}
}

// LikePattern turns a glob into a LIKE pattern for catalog checks: '*'
// becomes '%' and the LIKE metacharacters are escaped.
func TestLikePatternEscapesMetacharacters(t *testing.T) {
	cases := map[string]string{
		"test_*":    `test\_%`,
		"a%b":       `a\%b`,
		`back\sl*`:  `back\\sl%`,
		"exact":     "exact",
		"*_fixture": `%\_fixture`,
	}
	for in, want := range cases {
		if got := likePattern(in); got != want {
			t.Fatalf("likePattern(%q) = %q, want %q", in, got, want)
		}
	}
}
