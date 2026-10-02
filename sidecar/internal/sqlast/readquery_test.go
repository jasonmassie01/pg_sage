//go:build cgo

package sqlast

import (
	"errors"
	"testing"
)

func inspect(t *testing.T, sql string) ReadQuery {
	t.Helper()
	got, err := InspectReadQuery(sql)
	if err != nil {
		t.Fatalf("InspectReadQuery(%q) error = %v", sql, err)
	}
	return got
}

func hasName(names []QualifiedName, schema, name string) bool {
	for _, n := range names {
		if n.Schema == schema && n.Name == name {
			return true
		}
	}
	return false
}

func TestInspectReadQueryFindsEveryFunctionCall(t *testing.T) {
	cases := []struct {
		sql, schema, name string
	}{
		{"SELECT pg_terminate_backend(1)", "", "pg_terminate_backend"},
		{"SELECT pg_catalog.pg_sleep(1)", "pg_catalog", "pg_sleep"},
		{"SELECT * FROM t WHERE id = (SELECT nextval('s'))", "", "nextval"},
		{"WITH x AS (SELECT set_config('a','b',false)) SELECT * FROM x", "", "set_config"},
		{"SELECT * FROM generate_series(1, 3) g", "", "generate_series"},
		{"SELECT 1 FROM t ORDER BY random()", "", "random"},
		{"SELECT * FROM t, LATERAL (SELECT lo_import('/x')) l", "", "lo_import"},
		{"VALUES (dblink_exec('c', 'q'))", "", "dblink_exec"},
		{"SELECT count(*) FILTER (WHERE f(x)) FROM t", "", "f"},
		{`SELECT "PG_SLEEP"(1)`, "", "PG_SLEEP"},
		{`SELECT U&"pg\005fsleep"(1)`, "", "pg_sleep"},
		{"SELECT a FROM t GROUP BY a HAVING max(b) > evil()", "", "evil"},
		{"SELECT CASE WHEN true THEN app.side_effect() END", "app", "side_effect"},
	}
	for _, c := range cases {
		got := inspect(t, c.sql)
		if !hasName(got.Functions, c.schema, c.name) {
			t.Errorf("%q: functions = %v, want %s.%s", c.sql, got.Functions, c.schema, c.name)
		}
	}
}

func TestInspectReadQueryFindsOperators(t *testing.T) {
	cases := []struct {
		sql, schema, name string
	}{
		{"SELECT 1 + 2", "", "+"},
		{"SELECT 1 OPERATOR(public.===) 2", "public", "==="},
		{"SELECT * FROM t WHERE a BETWEEN 1 AND 2", "", ">="},
		{"SELECT * FROM t WHERE a BETWEEN 1 AND 2", "", "<="},
		{"SELECT * FROM t WHERE a IN (1, 2)", "", "="},
		{"SELECT * FROM t WHERE a IN (SELECT b FROM u)", "", "="},
		{"SELECT * FROM t WHERE a < ANY (SELECT b FROM u)", "", "<"},
		{"SELECT * FROM t WHERE name LIKE 'x%'", "", "~~"},
		{"SELECT CASE a WHEN 1 THEN 'one' END FROM t", "", "="},
		{"SELECT GREATEST(a, b) FROM t", "", ">"},
		{"SELECT NULLIF(a, b) FROM t", "", "="},
	}
	for _, c := range cases {
		got := inspect(t, c.sql)
		if !hasName(got.Operators, c.schema, c.name) {
			t.Errorf("%q: operators = %v, want %s.%s", c.sql, got.Operators, c.schema, c.name)
		}
	}
}

func TestInspectReadQueryFindsCastTargetsAndRelations(t *testing.T) {
	got := inspect(t, "SELECT 1::my_domain, 2::int, x::app.money2 FROM app.orders o "+
		"JOIN v_report r ON true")
	for _, want := range []QualifiedName{
		{"", "my_domain"}, {"pg_catalog", "int4"}, {"app", "money2"},
	} {
		if !hasName(got.Types, want.Schema, want.Name) {
			t.Errorf("types = %v, want %v", got.Types, want)
		}
	}
	if !hasName(got.Relations, "app", "orders") || !hasName(got.Relations, "", "v_report") {
		t.Errorf("relations = %v, want app.orders and v_report", got.Relations)
	}
	if len(got.Functions) != 0 {
		t.Errorf("functions = %v, want none", got.Functions)
	}
}

func TestInspectReadQueryRecordsCTENamesAndKeepsTheirReferences(t *testing.T) {
	got := inspect(t, "WITH recent AS (SELECT * FROM public.orders) SELECT * FROM recent")
	if len(got.CTENames) != 1 || got.CTENames[0] != "recent" {
		t.Fatalf("CTENames = %v, want [recent]", got.CTENames)
	}
	if !hasName(got.Relations, "", "recent") || !hasName(got.Relations, "public", "orders") {
		t.Errorf("relations = %v, want recent and public.orders", got.Relations)
	}
}

// t.fn is functional notation for fn(t) when t has no column fn.
func TestInspectReadQueryRecordsAttributeNotation(t *testing.T) {
	got := inspect(t, "SELECT o.total_with_tax, (o).audit_touch FROM orders o")
	for _, name := range []string{"total_with_tax", "audit_touch"} {
		if !hasName(got.AttributeCalls, "", name) {
			t.Errorf("attribute calls = %v, want %s", got.AttributeCalls, name)
		}
	}
	plain := inspect(t, "SELECT total FROM orders")
	if len(plain.AttributeCalls) != 0 {
		t.Errorf("unqualified column recorded as attribute call: %v", plain.AttributeCalls)
	}
}

func TestInspectReadQueryFlagsWritesAndLocks(t *testing.T) {
	cases := []struct {
		sql                     string
		locking, modifies, into bool
	}{
		{"SELECT * FROM t FOR UPDATE", true, false, false},
		{"SELECT * FROM t FOR NO KEY UPDATE SKIP LOCKED", true, false, false},
		{"SELECT * FROM (SELECT * FROM t FOR SHARE) s", true, false, false},
		{"SELECT * FROM t FOR KEY SHARE", true, false, false},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", false, true, false},
		{"WITH i AS (INSERT INTO t VALUES (1) RETURNING *) SELECT 1", false, true, false},
		{"WITH u AS (UPDATE t SET a = 1 RETURNING *) SELECT 1", false, true, false},
		{"SELECT 1 INTO new_table", false, false, true},
		{"SELECT a FROM t", false, false, false},
	}
	for _, c := range cases {
		got := inspect(t, c.sql)
		if got.LockingClause != c.locking || got.ModifiesData != c.modifies ||
			got.SelectInto != c.into {
			t.Errorf("%q: locking=%v modifies=%v into=%v, want %v %v %v", c.sql,
				got.LockingClause, got.ModifiesData, got.SelectInto,
				c.locking, c.modifies, c.into)
		}
	}
}

func TestInspectReadQueryDeduplicatesNames(t *testing.T) {
	got := inspect(t, "SELECT lower(a), lower(b), lower(c) FROM t")
	count := 0
	for _, f := range got.Functions {
		if f.Name == "lower" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("functions = %v, want lower once", got.Functions)
	}
}

func TestInspectReadQueryRejectsNonReadInput(t *testing.T) {
	for _, sql := range []string{
		"", "   ", "SELECT 1; SELECT 2", "DELETE FROM t", "not sql at all",
		"CREATE TABLE x (id int)", "EXPLAIN ANALYZE SELECT 1",
	} {
		_, err := InspectReadQuery(sql)
		if !errors.Is(err, ErrRejected) {
			t.Errorf("InspectReadQuery(%q) error = %v, want ErrRejected", sql, err)
		}
	}
}

func TestQualifiedNameString(t *testing.T) {
	if got := (QualifiedName{Name: "f"}).String(); got != "f" {
		t.Errorf("String() = %q, want f", got)
	}
	if got := (QualifiedName{Schema: "s", Name: "f"}).String(); got != "s.f" {
		t.Errorf("String() = %q, want s.f", got)
	}
}

// No concurrent access tests: InspectReadQuery is a pure function of its
// input and holds no shared state.
