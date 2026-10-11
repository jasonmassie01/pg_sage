package fleetlearn

import (
	"strings"
	"testing"
)

func TestNormalizeQuery_RedactsLiteralsAndIdentifiers(t *testing.T) {
	got := NormalizeQuery(`SELECT id, "Email" FROM public.users ` +
		`WHERE tenant_id = 42 AND email = 'bob@example.com' AND note = E'x\'y' ` +
		`AND body = $tag$secret$tag$ LIMIT 10`)
	for _, leaked := range []string{"users", "email", "tenant_id", "bob", "42",
		"secret", "public", "note", "body", "10", "x'y"} {
		if strings.Contains(strings.ToLower(got), leaked) {
			t.Fatalf("normalized query leaks %q: %s", leaked, got)
		}
	}
	want := "select x , x from x . x where x = ? and x = ? and x = ? and x = ? limit ?"
	if got != want {
		t.Fatalf("NormalizeQuery = %q, want %q", got, want)
	}
}

func TestNormalizeQuery_SameShapeDifferentNamesAndValuesMatch(t *testing.T) {
	a := NormalizeQuery("select * from orders where customer_id = $1 and total > 100")
	b := NormalizeQuery("SELECT *\n  FROM invoices WHERE account = $7 AND amount > 3.5")
	if a != b {
		t.Fatalf("same shape normalized differently:\n%q\n%q", a, b)
	}
	c := NormalizeQuery("select * from orders where customer_id = $1 order by total")
	if a == c {
		t.Fatal("different shapes normalized to the same text")
	}
}

func TestNormalizeQuery_InListsCollapse(t *testing.T) {
	a := NormalizeQuery("select a from t where b in (1, 2, 3)")
	b := NormalizeQuery("select a from t where b in ($1,$2)")
	if a != b {
		t.Fatalf("IN lists of different length differ: %q vs %q", a, b)
	}
}

func TestNormalizeQuery_EmptyAndCommentOnly(t *testing.T) {
	for _, in := range []string{"", "   ", "/* pg_sage */", "-- only a comment\n"} {
		if got := NormalizeQuery(in); got != "" {
			t.Fatalf("NormalizeQuery(%q) = %q, want empty", in, got)
		}
		if got := QueryShapeHash(in); got != "" {
			t.Fatalf("QueryShapeHash(%q) = %q, want empty", in, got)
		}
	}
}

func TestNormalizeQuery_UnterminatedLiteralDoesNotLeak(t *testing.T) {
	got := NormalizeQuery("select a from t where b = 'unterminated secret")
	if strings.Contains(got, "secret") || strings.Contains(got, "unterminated") {
		t.Fatalf("unterminated literal leaked: %q", got)
	}
}

func TestQueryShapeHash_StableAndDistinct(t *testing.T) {
	h1 := QueryShapeHash("select a from t where b = 1")
	h2 := QueryShapeHash("SELECT z FROM y WHERE q = 99")
	h3 := QueryShapeHash("select a from t where b = 1 order by a")
	if h1 == "" || h1 != h2 {
		t.Fatalf("equal shapes hash differently: %q %q", h1, h2)
	}
	if h1 == h3 {
		t.Fatal("distinct shapes share a hash")
	}
	if len(h1) != 16 {
		t.Fatalf("hash length = %d, want 16 hex chars", len(h1))
	}
}

func TestTableShapeHash_TypesOnlyOrderSensitive(t *testing.T) {
	a := TableShapeHash([]string{"bigint", "text", "timestamp with time zone"})
	b := TableShapeHash([]string{"bigint", "text", "timestamp with time zone"})
	c := TableShapeHash([]string{"text", "bigint", "timestamp with time zone"})
	if a == "" || a != b {
		t.Fatalf("same columns hash differently: %q %q", a, b)
	}
	if a == c {
		t.Fatal("column order is part of the shape")
	}
	if TableShapeHash(nil) != "" {
		t.Fatal("a table without columns has no shape")
	}
}

func TestIndexShapeHash_DependsOnEveryComponent(t *testing.T) {
	table := TableShapeHash([]string{"bigint", "text"})
	base := IndexShapeHash(table, "btree", false, false, []int{2})
	variants := map[string]string{
		"method":   IndexShapeHash(table, "hash", false, false, []int{2}),
		"unique":   IndexShapeHash(table, "btree", true, false, []int{2}),
		"partial":  IndexShapeHash(table, "btree", false, true, []int{2}),
		"keys":     IndexShapeHash(table, "btree", false, false, []int{1}),
		"multikey": IndexShapeHash(table, "btree", false, false, []int{2, 1}),
		"table":    IndexShapeHash(TableShapeHash([]string{"int"}), "btree", false, false, []int{2}),
	}
	for name, h := range variants {
		if h == base {
			t.Fatalf("index shape ignores %s", name)
		}
	}
	if IndexShapeHash(table, "btree", false, false, []int{2}) != base {
		t.Fatal("index shape is not deterministic")
	}
	if IndexShapeHash("", "btree", false, false, []int{1}) != "" {
		t.Fatal("an index of a shapeless table has no shape")
	}
}

func TestBoundary(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"operator fleet", nil, ""},
		{"tenant tag", []string{"tier=prod", "tenant=acme"}, "tenant:acme"},
		{"tenant tag colon", []string{"tenant:Acme"}, "tenant:acme"},
		{"empty tenant tag", []string{"tenant="}, ""},
		{"positional tags are not a tenant", []string{"guard", "local", "ten-9"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Boundary(tc.tags); got != tc.want {
				t.Fatalf("Boundary(%v) = %q, want %q", tc.tags, got, tc.want)
			}
		})
	}
}

func TestTableOfOutcome(t *testing.T) {
	cases := []struct {
		detail, object, sql, want string
	}{
		{"public.orders", "", "", "public.orders"},
		{"Orders", "", "", "public.orders"},
		{"", "app.items|btree(a)", "", "app.items"},
		{"", "app.items(a,b)", "", "app.items"},
		{"", "", "CREATE INDEX CONCURRENTLY idx ON app.events USING btree (ts)", "app.events"},
		{"", "", `create index if not exists "i" on "Sales"."Line" (x)`, "sales.line"},
		{"", "", "ALTER TABLE ONLY public.t SET (fillfactor=80)", "public.t"},
		{"", "", "VACUUM (ANALYZE) public.big", "public.big"},
		{"", "", "SET work_mem = '64MB'", ""},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		if got := TableOfOutcome(tc.detail, tc.object, tc.sql); got != tc.want {
			t.Fatalf("TableOfOutcome(%q,%q,%q) = %q, want %q",
				tc.detail, tc.object, tc.sql, got, tc.want)
		}
	}
}
