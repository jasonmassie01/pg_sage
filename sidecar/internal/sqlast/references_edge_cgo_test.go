//go:build cgo

package sqlast

import (
	"reflect"
	"testing"
)

func refsOf(t *testing.T, sql string) Refs {
	t.Helper()
	got, err := References(sql)
	if err != nil {
		t.Fatalf("References(%q) error = %v", sql, err)
	}
	return got
}

func pairs(rel QualifiedName, columns ...string) []ColumnPair {
	out := make([]ColumnPair, 0, len(columns))
	for _, column := range columns {
		out = append(out, ColumnPair{Relation: rel, Column: column})
	}
	return out
}

var (
	rvOrders = QualifiedName{Schema: "rv", Name: "orders"}
	rvItems  = QualifiedName{Schema: "rv", Name: "items"}
)

// Column lineage through alias lists, star expansion and CTE column lists.
func TestReferencesLineage(t *testing.T) {
	cases := []struct {
		name, sql string
		want      []ColumnPair
	}{
		{"subquery alias list", "SELECT x.b FROM (SELECT email FROM rv.orders) x(b)",
			pairs(rvOrders, "email")},
		{"CTE column list", "WITH c(x) AS (SELECT email FROM rv.orders) SELECT x FROM c",
			pairs(rvOrders, "email")},
		{"star over a subquery", "SELECT s.email FROM (SELECT * FROM rv.orders) s",
			pairs(rvOrders, "email")},
		{"star of a CTE with listed columns",
			"WITH c AS (SELECT id, email FROM rv.orders) SELECT * FROM c",
			pairs(rvOrders, "email", "id")},
		{"expression output has no lineage",
			"SELECT x.e FROM (SELECT lower(email) AS e FROM rv.orders) x",
			pairs(rvOrders, "email")},
		{"bare column falls through to the outer level",
			"SELECT o.id FROM rv.orders o WHERE EXISTS " +
				"(SELECT 1 FROM generate_series(1, 2) g(n) WHERE n = qty)",
			pairs(rvOrders, "id", "qty")},
		{"multi-assign SET", "UPDATE rv.orders SET (status, note) = ('a', 'b') WHERE id = 1",
			pairs(rvOrders, "id", "note", "status")},
		{"ON CONFLICT bare column is the target",
			"INSERT INTO rv.orders (id, n) VALUES (1, 1) ON CONFLICT (id) DO UPDATE SET n = n + 1",
			pairs(rvOrders, "id", "n")},
		{"tablesample", "SELECT email FROM rv.orders TABLESAMPLE SYSTEM (10)",
			pairs(rvOrders, "email")},
		{"group, having, window, distinct on",
			"SELECT DISTINCT ON (status) status, sum(amount) OVER (PARTITION BY region) " +
				"FROM rv.orders GROUP BY status, region, amount HAVING count(*) > min(qty)",
			pairs(rvOrders, "amount", "qty", "region", "status")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := refsOf(t, c.sql)
			if !reflect.DeepEqual(got.Columns, c.want) {
				t.Errorf("columns = %+v, want %+v (refs %+v)", got.Columns, c.want, got)
			}
			if len(got.Ambiguous) != 0 || len(got.Unresolved) != 0 {
				t.Errorf("unexpected ambiguous %v / unresolved %v", got.Ambiguous, got.Unresolved)
			}
		})
	}
}

func TestReferencesStarAndOpaqueEntries(t *testing.T) {
	got := refsOf(t, "SELECT * FROM rv.orders, rv.items")
	if !reflect.DeepEqual(got.Star, []QualifiedName{rvItems, rvOrders}) {
		t.Errorf("star = %v, want both relations", got.Star)
	}
	got = refsOf(t, "SELECT n FROM rv.orders, generate_series(1, 3) n")
	want := []AmbiguousColumn{{Column: "n",
		Candidates: []QualifiedName{{Name: "n"}, rvOrders}}}
	if !reflect.DeepEqual(got.Ambiguous, want) {
		t.Errorf("ambiguous = %+v, want %+v", got.Ambiguous, want)
	}
	got = refsOf(t, "SELECT generate_series FROM generate_series(1, 3)")
	if len(got.Columns) != 0 || len(got.Unresolved) != 0 || len(got.Ambiguous) != 0 {
		t.Errorf("function column attributed: %+v", got)
	}
	got = refsOf(t, "SELECT x, ordinality FROM rv.orders o, "+
		"unnest(o.tags) WITH ORDINALITY AS u(x)")
	if len(got.Ambiguous) != 2 {
		t.Errorf("ambiguous = %+v, want x and ordinality", got.Ambiguous)
	}
	got = refsOf(t, "SELECT q.* FROM rv.orders o")
	if !reflect.DeepEqual(got.Unresolved, []string{"q.*"}) || len(got.Star) != 0 {
		t.Errorf("unknown alias star = %+v", got)
	}
	got = refsOf(t, "SELECT rv.orders.* FROM rv.orders")
	if !reflect.DeepEqual(got.Star, []QualifiedName{rvOrders}) {
		t.Errorf("schema-qualified star = %v", got.Star)
	}
	got = refsOf(t, "SELECT email, jt.v FROM rv.orders, "+
		"JSON_TABLE(payload, '$' COLUMNS (v int PATH '$.v')) jt")
	if len(got.Ambiguous) != 1 || got.Ambiguous[0].Column != "email" ||
		len(got.Unresolved) != 0 {
		t.Errorf("a bare column next to JSON_TABLE must be ambiguous: %+v", got)
	}
}

func TestReferencesValuesAndSetOperations(t *testing.T) {
	got := refsOf(t, "SELECT v.a FROM (VALUES (1), (2)) v(a)")
	if len(got.Columns) != 0 || len(got.Relations) != 0 || len(got.Unresolved) != 0 {
		t.Errorf("VALUES derived table: %+v", got)
	}
	got = refsOf(t, "SELECT id FROM rv.orders WHERE id IN "+
		"(SELECT order_id FROM rv.items EXCEPT SELECT order_id FROM rv.archive) LIMIT 5")
	want := append(pairs(QualifiedName{Schema: "rv", Name: "archive"}, "order_id"),
		append(pairs(rvItems, "order_id"), pairs(rvOrders, "id")...)...)
	if !reflect.DeepEqual(got.Columns, want) {
		t.Errorf("columns = %+v, want %+v", got.Columns, want)
	}
}

func TestReferencesWritesInCTEs(t *testing.T) {
	got := refsOf(t, "WITH i AS (INSERT INTO rv.log (id) VALUES (1) "+
		"ON CONFLICT (id) DO NOTHING RETURNING id), "+
		"m AS (MERGE INTO rv.orders o USING rv.items s ON o.id = s.order_id "+
		"WHEN MATCHED THEN DELETE) SELECT id FROM i")
	log := QualifiedName{Schema: "rv", Name: "log"}
	if got.Kind != StmtSelect || got.Target != nil || !got.ModifiesData ||
		got.LockMode != LockRowExclusive {
		t.Errorf("refs = %+v, want a select that modifies data", got)
	}
	wantLocks := []RelationLock{{rvItems, LockAccessShare}, {log, LockRowExclusive},
		{rvOrders, LockRowExclusive}}
	if !reflect.DeepEqual(got.Locks, wantLocks) {
		t.Errorf("locks = %+v, want %+v", got.Locks, wantLocks)
	}
}

func TestReferencesUtilityAndNestedStatements(t *testing.T) {
	got := refsOf(t, "EXPLAIN SELECT email FROM rv.orders")
	if got.Kind != StmtOther || !reflect.DeepEqual(got.Relations, []QualifiedName{rvOrders}) ||
		got.LockMode != LockNone || got.Locks != nil {
		t.Errorf("EXPLAIN refs = %+v", got)
	}
	got = refsOf(t, "SELECT (SELECT max(id) FROM rv.items) FROM rv.orders")
	if len(got.Relations) != 2 || len(got.Unresolved) != 0 {
		t.Errorf("scalar subquery in target: %+v", got)
	}
}
