package migration

import (
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/pg-sage/sidecar/internal/migration/ddlrules"
)

// These drive the forms the online column-swap planner emits
// (migration-09b) through hand-built trees, so they run on every build.

// No concurrent access tests here: astMapper is a value with no shared
// state; TestASTStatementConcurrentCallers covers the parser path.

func updateNode(rv *pg_query.RangeVar) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_UpdateStmt{
		UpdateStmt: &pg_query.UpdateStmt{Relation: rv}}}
}

func lockNode(mode int32, rels ...*pg_query.RangeVar) *pg_query.Node {
	nodes := make([]*pg_query.Node, len(rels))
	for i, rv := range rels {
		nodes[i] = &pg_query.Node{Node: &pg_query.Node_RangeVar{RangeVar: rv}}
	}
	return &pg_query.Node{Node: &pg_query.Node_LockStmt{
		LockStmt: &pg_query.LockStmt{Relations: nodes, Mode: mode}}}
}

// UPDATE is DML, outside the DDL classifier: it stays unrecognized with
// or without a target, as the regex rules leave it (the equivalence
// corpus pins "UPDATE t SET x = 1" to no rule). The swap planner states
// the backfill's ROW EXCLUSIVE itself.
func TestASTTreeUpdateStaysUnrecognized(t *testing.T) {
	for _, rv := range []*pg_query.RangeVar{rangeVar("app", "t"), nil} {
		if found, known := testMapper.statement(updateNode(rv)); known || len(found) != 0 {
			t.Fatalf("UPDATE: rules %v known %v, want unrecognized", ruleIDs(found), known)
		}
	}
}

// Boundary: every PostgreSQL lock mode maps to its name; 0 and 9 are not
// modes, so the statement is unrecognized.
func TestASTTreeLockTableModes(t *testing.T) {
	modes := map[int32]string{
		1: "ACCESS SHARE", 2: "ROW SHARE", 3: "ROW EXCLUSIVE",
		4: "SHARE UPDATE EXCLUSIVE", 5: "SHARE", 6: "SHARE ROW EXCLUSIVE",
		7: "EXCLUSIVE", 8: "ACCESS EXCLUSIVE",
	}
	for mode, want := range modes {
		found, known := testMapper.statement(lockNode(mode, rangeVar("app", "t")))
		if !known {
			t.Fatalf("LOCK mode %d unrecognized", mode)
		}
		requireOneRule(t, found, "ddl_lock_table", want)
	}
	for _, mode := range []int32{0, 9, -1} {
		if found, known := testMapper.statement(lockNode(mode, rangeVar("app", "t"))); known {
			t.Fatalf("LOCK mode %d known: %v", mode, ruleIDs(found))
		}
	}
	if found, known := testMapper.statement(lockNode(8)); known {
		t.Fatalf("LOCK with no relation known: %v", ruleIDs(found))
	}
}

// State: several tables in one LOCK name the first; the lock is the
// statement's mode.
func TestASTTreeLockTableSeveralRelations(t *testing.T) {
	found, known := testMapper.statement(lockNode(8, rangeVar("app", "t"), rangeVar("app", "c")))
	if !known {
		t.Fatal("multi-table LOCK unrecognized")
	}
	requireOneRule(t, found, "ddl_lock_table", lockAccessExclusive)
}

func dropNode(kind pg_query.ObjectType, obj *pg_query.Node) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_DropStmt{DropStmt: &pg_query.DropStmt{
		RemoveType: kind, Objects: []*pg_query.Node{obj}}}}
}

func TestASTTreeDropTriggerNamesItsTable(t *testing.T) {
	obj := pg_query.MakeListNode([]*pg_query.Node{strNode("app"), strNode("t"), strNode("trg")})
	found, known := testMapper.statement(dropNode(pg_query.ObjectType_OBJECT_TRIGGER, obj))
	if !known {
		t.Fatal("DROP TRIGGER unrecognized")
	}
	requireOneRule(t, found, "ddl_drop_trigger", lockAccessExclusive)

	// Unqualified table: the schema is unknown, the table is named.
	obj = pg_query.MakeListNode([]*pg_query.Node{strNode("t"), strNode("trg")})
	found, _ = testMapper.statement(dropNode(pg_query.ObjectType_OBJECT_TRIGGER, obj))
	if len(found) != 1 || found[0].TableName != "t" || found[0].SchemaName != "" {
		t.Fatalf("unqualified DROP TRIGGER: %+v", found)
	}
}

func TestASTTreeDropFunction(t *testing.T) {
	obj := &pg_query.Node{Node: &pg_query.Node_ObjectWithArgs{
		ObjectWithArgs: &pg_query.ObjectWithArgs{
			Objname: []*pg_query.Node{strNode("app"), strNode("f")}}}}
	found, known := testMapper.statement(dropNode(pg_query.ObjectType_OBJECT_FUNCTION, obj))
	if !known || len(found) != 1 || found[0].RuleID != "ddl_drop_function" {
		t.Fatalf("DROP FUNCTION: %v known %v", ruleIDs(found), known)
	}
	if found[0].LockLevel != lockAccessExclusive || found[0].TableName != "" {
		t.Fatalf("DROP FUNCTION %+v, want ACCESS EXCLUSIVE on no table", found[0])
	}
	// Other drops stay unrecognized: they remove an object pg_sage does
	// not measure.
	if found, known := testMapper.statement(
		dropNode(pg_query.ObjectType_OBJECT_VIEW, obj)); known {
		t.Fatalf("DROP VIEW known: %v", ruleIDs(found))
	}
}

func createFunctionNode(replace, procedure bool) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_CreateFunctionStmt{
		CreateFunctionStmt: &pg_query.CreateFunctionStmt{
			Replace: replace, IsProcedure: procedure,
			Funcname: []*pg_query.Node{strNode("app"), strNode("f")}}}}
}

// A new function takes no table lock; OR REPLACE can change what live
// triggers and defaults run, so it is not proven safe.
func TestASTTreeCreateFunction(t *testing.T) {
	found, known := testMapper.statement(createFunctionNode(false, false))
	if !known || len(found) != 0 {
		t.Fatalf("CREATE FUNCTION: %v known %v, want a safe form", ruleIDs(found), known)
	}
	if found, known := testMapper.statement(createFunctionNode(true, false)); known {
		t.Fatalf("CREATE OR REPLACE FUNCTION known: %v", ruleIDs(found))
	}
	if found, known := testMapper.statement(createFunctionNode(false, true)); known {
		t.Fatalf("CREATE PROCEDURE known: %v", ruleIDs(found))
	}
}

func alterSeqNode(opts ...string) *pg_query.Node {
	var nodes []*pg_query.Node
	for _, o := range opts {
		nodes = append(nodes, pg_query.MakeSimpleDefElemNode(o, nil, 0))
	}
	return &pg_query.Node{Node: &pg_query.Node_AlterSeqStmt{AlterSeqStmt: &pg_query.AlterSeqStmt{
		Sequence: rangeVar("app", "s"), Options: nodes}}}
}

// Only AS and OWNED BY are known: RESTART, INCREMENT and the rest change
// the values a live default hands out.
func TestASTTreeAlterSequence(t *testing.T) {
	for _, opts := range [][]string{{"owned_by"}, {"as"}, {"as", "owned_by"}} {
		if found, known := testMapper.statement(alterSeqNode(opts...)); !known || len(found) != 0 {
			t.Fatalf("ALTER SEQUENCE %v: %v known %v, want a safe form",
				opts, ruleIDs(found), known)
		}
	}
	for _, opts := range [][]string{{"restart"}, {"owned_by", "increment"}, {}} {
		if found, known := testMapper.statement(alterSeqNode(opts...)); known {
			t.Fatalf("ALTER SEQUENCE %v known: %v", opts, ruleIDs(found))
		}
	}
}

// The new rules are in the one lock table with the lock the planner
// states (migration-01: the preflight reads lock levels from here).
func TestSwapRulesInCatalog(t *testing.T) {
	want := map[string]string{
		"ddl_lock_table": "ACCESS EXCLUSIVE", "ddl_drop_trigger": "ACCESS EXCLUSIVE",
		"ddl_drop_function": "ACCESS EXCLUSIVE",
	}
	for id, lock := range want {
		r, ok := ddlrules.Lookup(id)
		if !ok || r.LockLevel != lock || r.RequiresRewrite {
			t.Fatalf("Lookup(%s) = %+v, %v; want lock %s, no rewrite", id, r, ok, lock)
		}
	}
}

// The regex rules have no pattern for the swap forms: on a build without
// the parser these statements stay unrecognized (fail closed), and the
// advisor's regex path reports nothing new for them.
func TestRegexRulesIgnoreSwapForms(t *testing.T) {
	for _, sql := range []string{
		`UPDATE app.t SET a = 1`, `LOCK TABLE app.t IN ACCESS EXCLUSIVE MODE`,
		`DROP TRIGGER trg ON app.t`, `DROP FUNCTION app.f()`,
	} {
		if found := NewRegexClassifier().Classify(sql, 17); len(found) != 0 {
			t.Fatalf("%q: regex rules %v, want none", sql, ruleIDs(found))
		}
	}
}

// Risk: a dropped trigger or function is metadata-only, like DROP INDEX.
func TestSwapRulesRewriteWeight(t *testing.T) {
	for _, id := range []string{"ddl_drop_trigger", "ddl_drop_function"} {
		if got := rewriteWeight(&DDLRisk{RuleID: id}); got != 0.2 {
			t.Fatalf("rewriteWeight(%s) = %v, want 0.2", id, got)
		}
	}
}
