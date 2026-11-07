//go:build cgo

package sqlast

import (
	"errors"
	"reflect"
	"testing"
)

// No concurrent access tests: InspectStatements is a pure function of its
// argument and shares no state.

func inspectOne(t *testing.T, sql string) Statement {
	t.Helper()
	got, err := InspectStatements(sql)
	if err != nil {
		t.Fatalf("InspectStatements(%q): %v", sql, err)
	}
	if len(got) != 1 {
		t.Fatalf("InspectStatements(%q) = %d statements, want 1", sql, len(got))
	}
	return got[0]
}

func TestInspectStatementsDescribesAScript(t *testing.T) {
	script := `SET lock_timeout = '5s';
BEGIN;
ALTER TABLE public.orders ADD COLUMN status_new text;
COMMIT;
CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders (lower(status_new));
RESET lock_timeout;`
	got, err := InspectStatements(script)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, len(got))
	for i, st := range got {
		kinds[i] = st.Kind
	}
	want := []string{KindSet, KindBegin, KindAlterTable, KindCommit, KindCreateIndex, KindReset}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	orders := []QualifiedName{{Schema: "public", Name: "orders"}}
	if !reflect.DeepEqual(got[2].Relations, orders) || !got[2].TableTarget {
		t.Errorf("ALTER relations = %v table=%v", got[2].Relations, got[2].TableTarget)
	}
	if !reflect.DeepEqual(got[2].Commands, []string{"AT_AddColumn"}) {
		t.Errorf("ALTER commands = %v", got[2].Commands)
	}
	if !reflect.DeepEqual(got[4].Relations, orders) {
		t.Errorf("CREATE INDEX relations = %v", got[4].Relations)
	}
	if !reflect.DeepEqual(got[4].Functions, []QualifiedName{{Name: "lower"}}) {
		t.Errorf("CREATE INDEX functions = %v", got[4].Functions)
	}
	if got[0].Setting != "lock_timeout" || got[5].Setting != "lock_timeout" {
		t.Errorf("settings = %q, %q", got[0].Setting, got[5].Setting)
	}
}

func TestInspectStatementsFoldsAndKeepsQuotedNames(t *testing.T) {
	st := inspectOne(t, `ALTER TABLE "Orders" ADD COLUMN x int`)
	if !reflect.DeepEqual(st.Relations, []QualifiedName{{Name: "Orders"}}) {
		t.Errorf("quoted relation = %v, want Orders kept", st.Relations)
	}
	st = inspectOne(t, `ALTER TABLE ORDERS ADD COLUMN x int`)
	if !reflect.DeepEqual(st.Relations, []QualifiedName{{Name: "orders"}}) {
		t.Errorf("unquoted relation = %v, want folded orders", st.Relations)
	}
}

func TestInspectStatementsSeesEveryRelation(t *testing.T) {
	st := inspectOne(t, `ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (c)
		REFERENCES customers (id) NOT VALID`)
	want := []QualifiedName{{Name: "customers"}, {Name: "orders"}}
	if !reflect.DeepEqual(st.Relations, want) {
		t.Errorf("FK relations = %v, want %v", st.Relations, want)
	}
	st = inspectOne(t, `UPDATE orders SET c = a.c FROM accounts a WHERE a.id = orders.id`)
	if len(st.Relations) != 2 || st.Kind != KindUpdate {
		t.Errorf("UPDATE FROM = %+v, want two relations", st)
	}
}

func TestInspectStatementsDropsCTEReferences(t *testing.T) {
	st := inspectOne(t, `WITH batch AS (SELECT id FROM orders LIMIT 1000)
		UPDATE orders SET c = 1 WHERE id IN (SELECT id FROM batch)`)
	if !reflect.DeepEqual(st.Relations, []QualifiedName{{Name: "orders"}}) {
		t.Errorf("relations = %v, want only orders", st.Relations)
	}
	if st.NestedWrite {
		t.Error("a read-only CTE is not a nested write")
	}
}

// A CTE name hides a real table only where the CTE is in scope: never
// for the UPDATE target, its own (non-recursive) body, or a sibling scope.
func TestInspectStatementsKeepsTablesACTENameShadows(t *testing.T) {
	accounts := QualifiedName{Name: "accounts"}
	for _, sql := range []string{
		`WITH accounts AS (SELECT 1) UPDATE accounts SET c = 1`,
		`WITH accounts AS (SELECT * FROM accounts) UPDATE orders SET c = 1`,
		`WITH a AS (SELECT * FROM accounts), accounts AS (SELECT 1)
			UPDATE orders SET c = 1 FROM a`,
		`UPDATE orders SET c = (SELECT c FROM accounts LIMIT 1)
			WHERE id IN (WITH accounts AS (SELECT 1 AS id) SELECT id FROM accounts)`,
	} {
		st := inspectOne(t, sql)
		found := false
		for _, rel := range st.Relations {
			found = found || rel == accounts
		}
		if !found {
			t.Errorf("%s: relations = %v, want the table accounts kept", sql, st.Relations)
		}
	}
}

func TestInspectStatementsFlagsNestedWrites(t *testing.T) {
	for _, sql := range []string{
		`WITH d AS (DELETE FROM orders RETURNING id) UPDATE orders SET c = 1`,
		`WITH u AS (UPDATE orders SET c = 2 RETURNING id) UPDATE orders SET c = 1`,
		`WITH i AS (INSERT INTO orders VALUES (1) RETURNING id) UPDATE orders SET c = 1`,
	} {
		if st := inspectOne(t, sql); !st.NestedWrite {
			t.Errorf("%s: NestedWrite = false", sql)
		}
	}
	if st := inspectOne(t, `UPDATE orders SET c = 1`); st.NestedWrite {
		t.Error("a plain UPDATE is not a nested write")
	}
}

func TestInspectStatementsReportsOtherKinds(t *testing.T) {
	cases := map[string]string{
		"DROP TABLE orders":                            "DropStmt",
		"GRANT SELECT ON orders TO PUBLIC":             "GrantStmt",
		"VACUUM orders":                                "VacuumStmt",
		"DO $$ BEGIN PERFORM 1; END $$":                "DoStmt",
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE": "VariableSetStmt",
		"ROLLBACK": "TransactionStmt",
		"SELECT 1": "SelectStmt",
	}
	for sql, node := range cases {
		st := inspectOne(t, sql)
		if st.Kind != KindOther || st.Node != node {
			t.Errorf("%s = kind %q node %q, want other/%s", sql, st.Kind, st.Node, node)
		}
	}
}

func TestInspectStatementsAlterOnNonTables(t *testing.T) {
	if st := inspectOne(t, "ALTER INDEX orders_idx SET (fillfactor = 90)"); st.TableTarget {
		t.Error("ALTER INDEX reported as a table target")
	}
	st := inspectOne(t, "ALTER TABLE orders RENAME COLUMN a TO b")
	if st.Kind != KindRename || !st.TableTarget ||
		!reflect.DeepEqual(st.Commands, []string{"OBJECT_COLUMN"}) {
		t.Errorf("RENAME COLUMN = %+v", st)
	}
	st = inspectOne(t, "ALTER TABLE orders RENAME TO orders_old")
	if !reflect.DeepEqual(st.Commands, []string{"OBJECT_TABLE"}) {
		t.Errorf("RENAME TABLE commands = %v", st.Commands)
	}
}

func TestInspectStatementsSettings(t *testing.T) {
	if st := inspectOne(t, "RESET ALL"); st.Kind != KindReset || st.Setting != "" {
		t.Errorf("RESET ALL = %+v, want reset with no setting", st)
	}
	if st := inspectOne(t, "SET LOCAL statement_timeout TO DEFAULT"); st.Kind != KindSet ||
		st.Setting != "statement_timeout" {
		t.Errorf("SET TO DEFAULT = %+v", st)
	}
	if st := inspectOne(t, "ANALYZE orders"); st.Kind != KindAnalyze ||
		!reflect.DeepEqual(st.Relations, []QualifiedName{{Name: "orders"}}) {
		t.Errorf("ANALYZE = %+v", st)
	}
}

func TestInspectStatementsInvalidAndEmpty(t *testing.T) {
	if _, err := InspectStatements("ALTER TABLE"); !errors.Is(err, ErrRejected) {
		t.Errorf("parse error = %v, want ErrRejected", err)
	}
	got, err := InspectStatements("  ")
	if err != nil || len(got) != 0 {
		t.Errorf("empty script = %v, %v; want no statements", got, err)
	}
	got, err = InspectStatements("-- only a comment")
	if err != nil || len(got) != 0 {
		t.Errorf("comment-only script = %v, %v; want no statements", got, err)
	}
}

// x.name runs name(x) when x has no column name, so attribute notation is
// a function call the parser does not mark as one (security-05 review).
func TestInspectStatementsReportsAttributeCalls(t *testing.T) {
	st := inspectOne(t, `UPDATE orders SET c = (42).pg_terminate_backend,
		n = ('/etc/passwd'::text).pg_read_file WHERE orders.id = 1`)
	want := []QualifiedName{{Name: "id"}, {Name: "pg_read_file"},
		{Name: "pg_terminate_backend"}}
	if !reflect.DeepEqual(st.AttributeCalls, want) {
		t.Errorf("attribute calls = %v, want %v", st.AttributeCalls, want)
	}
	if len(st.Functions) != 0 {
		t.Errorf("functions = %v, want none (no FuncCall node)", st.Functions)
	}
	if st = inspectOne(t, `UPDATE orders SET c = id`); len(st.AttributeCalls) != 0 {
		t.Errorf("bare column refs = %v, want no attribute calls", st.AttributeCalls)
	}
}
