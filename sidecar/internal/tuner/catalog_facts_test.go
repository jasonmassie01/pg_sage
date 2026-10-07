package tuner

import (
	"context"
	"testing"
)

func sampleFacts() *CatalogFacts {
	return &CatalogFacts{
		Tables: map[string]int64{
			"public.orders": 500000, "app.orders": 10, "public.users": 50000,
		},
		Indexes: map[string][]IndexFact{
			"public.orders": {{Name: "orders_customer_idx", LeadingColumn: "customer_id"},
				{Name: "orders_status_idx", LeadingColumn: "status"}},
			"public.users": {{Name: "users_email_idx", LeadingColumn: "email"}},
		},
	}
}

// Plain EXPLAIN omits the schema: an unqualified relation resolves only
// when exactly one schema has it.
func TestCatalogFacts_TableRowsResolution(t *testing.T) {
	f := sampleFacts()
	if rows, ok := f.TableRows("", "users"); !ok || rows != 50000 {
		t.Fatalf("unique unqualified = %d %t", rows, ok)
	}
	if _, ok := f.TableRows("", "orders"); ok {
		t.Fatal("ambiguous unqualified relation resolved")
	}
	if rows, ok := f.TableRows("app", "orders"); !ok || rows != 10 {
		t.Fatalf("qualified = %d %t", rows, ok)
	}
	if _, ok := f.TableRows("", "missing"); ok {
		t.Fatal("unknown relation resolved")
	}
	var nilFacts *CatalogFacts
	if _, ok := nilFacts.TableRows("public", "users"); ok {
		t.Fatal("nil facts resolved a table")
	}
	if _, ok := (&CatalogFacts{}).TableRows("public", "users"); ok {
		t.Fatal("empty facts resolved a table")
	}
}

func TestCatalogFacts_UsableIndexFilters(t *testing.T) {
	f := sampleFacts()
	cases := []struct {
		filter, want string
	}{
		{"(customer_id = 42)", "orders_customer_idx"},
		{"(o.customer_id = $1)", "orders_customer_idx"},
		{"(customer_id >= 10)", "orders_customer_idx"},
		{"(customer_id = ANY ('{1,2}'::integer[]))", "orders_customer_idx"},
		{"((status)::text = 'open'::text)", "orders_status_idx"},
		{"((note = 'x'::text) AND (customer_id = 7))", "orders_customer_idx"},
		{"(\"customer_id\" = 42)", "orders_customer_idx"},
		{"(lower(status) = 'x'::text)", ""},
		{"((customer_id = 1) OR (customer_id = 2))", ""},
		{"customer_id = 1 OR note = 'x'", ""},
		{"(note ~~ '%ab%'::text)", ""},
		{"(customer_id IS NULL)", ""},
		{"('customer_id = 1'::text = note)", ""},
		{"(other_id = 42)", ""},
		{"", ""},
		{"(((", ""},
	}
	for _, c := range cases {
		got, ok := f.UsableIndex("public", "orders", c.filter)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("UsableIndex(%q) = %q %t, want %q", c.filter, got, ok, c.want)
		}
	}
	if _, ok := f.UsableIndex("", "orders", "(customer_id = 1)"); ok {
		t.Error("ambiguous relation produced an index")
	}
	if _, ok := f.UsableIndex("public", "missing", "(customer_id = 1)"); ok {
		t.Error("unknown relation produced an index")
	}
}

// A HashJoin hint needs two or more plan aliases; a seq-scan hint needs a
// usable index; identifiers pg_hint_plan would need quoted are skipped
// rather than emitted raw. No prescription is ever an empty directive.
func TestPrescribe_RequiresRealIdentifiers(t *testing.T) {
	cases := []struct {
		name string
		s    PlanSymptom
		want string
	}{
		{"two aliases", PlanSymptom{Kind: SymptomBadNestedLoop,
			JoinAliases: []string{"a", "b"}}, "HashJoin(a b)"},
		{"three aliases", PlanSymptom{Kind: SymptomBadNestedLoop,
			JoinAliases: []string{"a", "b", "c"}}, "HashJoin(a b c)"},
		{"one alias", PlanSymptom{Kind: SymptomBadNestedLoop,
			JoinAliases: []string{"a"}}, ""},
		{"no alias", PlanSymptom{Kind: SymptomBadNestedLoop}, ""},
		{"quoted alias", PlanSymptom{Kind: SymptomBadNestedLoop,
			JoinAliases: []string{"a", "Bad Alias"}}, ""},
		{"index", PlanSymptom{Kind: SymptomSeqScanWithIndex, Alias: "o",
			IndexName: "orders_customer_idx"}, "IndexScan(o orders_customer_idx)"},
		{"no index", PlanSymptom{Kind: SymptomSeqScanWithIndex, Alias: "o"}, ""},
		{"no alias index", PlanSymptom{Kind: SymptomSeqScanWithIndex,
			RelationName: "orders", IndexName: "orders_customer_idx"},
			"IndexScan(orders orders_customer_idx)"},
		{"odd index name", PlanSymptom{Kind: SymptomSeqScanWithIndex, Alias: "o",
			IndexName: "Idx\"x"}, ""},
	}
	for _, c := range cases {
		rx := Prescribe(c.s, TunerConfig{})
		got := ""
		if rx != nil {
			got = rx.HintDirective
			if got == "" {
				t.Errorf("%s: empty directive prescribed", c.name)
			}
		}
		if got != c.want {
			t.Errorf("%s: hint = %q, want %q", c.name, got, c.want)
		}
	}
}

// LoadCatalogFacts reads real catalog rows: only valid, non-partial btree
// indexes on a plain leading column count.
func TestLoadCatalogFacts_DB(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	stmts := []string{
		"DROP SCHEMA IF EXISTS factsfx CASCADE",
		"CREATE SCHEMA factsfx",
		"CREATE TABLE factsfx.t (id int PRIMARY KEY, a int, b text, c int)",
		"CREATE TABLE factsfx.other (id int PRIMARY KEY)",
		"INSERT INTO factsfx.t SELECT i, i % 7, i::text, i FROM generate_series(1, 3000) i",
		"CREATE INDEX t_a_idx ON factsfx.t (a)",
		"CREATE INDEX t_b_partial ON factsfx.t (b) WHERE a = 1",
		"CREATE INDEX t_expr ON factsfx.t (lower(b))",
		"CREATE INDEX t_c_hash ON factsfx.t USING hash (c)",
		"ANALYZE factsfx.t",
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS factsfx CASCADE")
	})
	facts, err := LoadCatalogFacts(ctx, pool, []string{"t"})
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if _, ok := facts.TableRows("factsfx", "other"); ok {
		t.Fatal("a relation no plan names was loaded")
	}
	if rows, ok := facts.TableRows("factsfx", "t"); !ok || rows != 3000 {
		t.Fatalf("rows = %d %t, want 3000", rows, ok)
	}
	got := map[string]string{}
	for _, idx := range facts.Indexes["factsfx.t"] {
		got[idx.Name] = idx.LeadingColumn
	}
	want := map[string]string{"t_pkey": "id", "t_a_idx": "a"}
	if len(got) != len(want) || got["t_pkey"] != "id" || got["t_a_idx"] != "a" {
		t.Fatalf("indexes = %v, want %v", got, want)
	}
	for _, s := range []string{"sage", "pg_catalog", "information_schema"} {
		for k := range facts.Tables {
			if len(k) > len(s) && k[:len(s)+1] == s+"." {
				t.Fatalf("system/sage table in facts: %s", k)
			}
		}
	}
}

func TestLoadCatalogFacts_NilPool(t *testing.T) {
	facts, err := LoadCatalogFacts(context.Background(), nil, []string{"t"})
	if err != nil || facts == nil || len(facts.Tables) != 0 {
		t.Fatalf("nil pool = %+v %v, want empty facts", facts, err)
	}
}

func TestLoadCatalogFacts_CanceledContextIsAnError(t *testing.T) {
	pool, _ := requireTunerDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadCatalogFacts(ctx, pool, []string{"t"}); err == nil {
		t.Fatal("canceled load returned no error")
	}
}
