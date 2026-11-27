package migration

import (
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// These tests drive the parse-tree mapping with hand-built trees, so they
// run on every build (with or without cgo); classifier_ast_test.go runs
// the same forms through the real parser.

// No concurrent access tests here: astMapper is a value with no shared
// state; TestASTStatementConcurrentCallers covers the parser path.

func rangeVar(schema, name string) *pg_query.RangeVar {
	return &pg_query.RangeVar{Schemaname: schema, Relname: name, Inh: true}
}

func strNode(s string) *pg_query.Node { return pg_query.MakeStrNode(s) }

func alterStmt(cmds ...*pg_query.AlterTableCmd) *pg_query.Node {
	nodes := make([]*pg_query.Node, len(cmds))
	for i, c := range cmds {
		nodes[i] = &pg_query.Node{Node: &pg_query.Node_AlterTableCmd{AlterTableCmd: c}}
	}
	return &pg_query.Node{Node: &pg_query.Node_AlterTableStmt{
		AlterTableStmt: &pg_query.AlterTableStmt{Relation: rangeVar("app", "t"), Cmds: nodes,
			Objtype: pg_query.ObjectType_OBJECT_TABLE}}}
}

func addColumnStmt(def *pg_query.Node) *pg_query.Node {
	return alterStmt(cmd(pg_query.AlterTableType_AT_AddColumn, def))
}

func cmd(sub pg_query.AlterTableType, def *pg_query.Node) *pg_query.AlterTableCmd {
	return &pg_query.AlterTableCmd{Subtype: sub, Def: def}
}

func constraintNode(c *pg_query.Constraint) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_Constraint{Constraint: c}}
}

func typeName(names ...string) *pg_query.TypeName {
	t := &pg_query.TypeName{}
	for _, n := range names {
		t.Names = append(t.Names, strNode(n))
	}
	return t
}

func columnDefNode(typ *pg_query.TypeName, cons ...*pg_query.Constraint) *pg_query.Node {
	def := &pg_query.ColumnDef{Colname: "c", TypeName: typ}
	for _, c := range cons {
		def.Constraints = append(def.Constraints, constraintNode(c))
	}
	return &pg_query.Node{Node: &pg_query.Node_ColumnDef{ColumnDef: def}}
}

func ruleIDs(cs []DDLClassification) []string {
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.RuleID)
	}
	return ids
}

// requireOneRule asserts found is exactly one rule on app.t with lock.
func requireOneRule(t *testing.T, found []DDLClassification, id, lock string) DDLClassification {
	t.Helper()
	if len(found) != 1 || found[0].RuleID != id {
		t.Fatalf("rules = %v, want [%s]", ruleIDs(found), id)
	}
	c := found[0]
	if c.LockLevel != lock || c.SchemaName != "app" || c.TableName != "t" {
		t.Fatalf("classification %+v, want lock %s on app.t", c, lock)
	}
	if c.Description == "" || c.SafeAlternative == "" || c.Statement != "stmt" {
		t.Fatalf("classification %+v lacks the rule's text or the statement", c)
	}
	return c
}

var testMapper = astMapper{stmt: "stmt", major: 17}

func TestASTTreeSetLoggedRewrites(t *testing.T) {
	for _, sub := range []pg_query.AlterTableType{
		pg_query.AlterTableType_AT_SetLogged, pg_query.AlterTableType_AT_SetUnLogged,
	} {
		found, known := testMapper.statement(alterStmt(cmd(sub, nil)))
		if !known {
			t.Fatalf("%v unrecognized", sub)
		}
		c := requireOneRule(t, found, "ddl_set_logged", lockAccessExclusive)
		if !c.RequiresRewrite {
			t.Fatalf("%v does not rewrite: %+v", sub, c)
		}
	}
}

// Boundary: SET ACCESS METHOD exists from PG 15; on PG 14 it is a syntax
// error, so it is unrecognized there, and an unknown server is flagged.
func TestASTTreeSetAccessMethodVersion(t *testing.T) {
	stmt := alterStmt(cmd(pg_query.AlterTableType_AT_SetAccessMethod, nil))
	for major, want := range map[int]bool{14: false, 15: true, 16: true, 18: true, 0: true} {
		m := astMapper{stmt: "stmt", major: major}
		found, known := m.statement(stmt)
		if known != want || (len(found) == 1) != want {
			t.Fatalf("PG %d: rules %v known %v, want known %v", major, ruleIDs(found), known, want)
		}
		if want {
			requireOneRule(t, found, "ddl_set_access_method", lockAccessExclusive)
		}
	}
}

func TestASTTreeDetachPartition(t *testing.T) {
	part := func(concurrent bool) *pg_query.Node {
		return &pg_query.Node{Node: &pg_query.Node_PartitionCmd{
			PartitionCmd: &pg_query.PartitionCmd{Name: rangeVar("app", "p1"),
				Concurrent: concurrent}}}
	}
	found, known := testMapper.statement(alterStmt(
		cmd(pg_query.AlterTableType_AT_DetachPartition, part(false))))
	if !known {
		t.Fatal("plain DETACH PARTITION unrecognized")
	}
	requireOneRule(t, found, "ddl_detach_partition_not_concurrent", lockAccessExclusive)
	found, known = astMapper{stmt: "stmt", major: 14}.statement(alterStmt(
		cmd(pg_query.AlterTableType_AT_DetachPartition, part(true))))
	if !known || len(found) != 0 {
		t.Fatalf("DETACH CONCURRENTLY on PG 14: rules %v known %v, want safe",
			ruleIDs(found), known)
	}
}

// State: an inline UNIQUE, REFERENCES or CHECK on ADD COLUMN builds an
// index or scans, and the statement's ACCESS EXCLUSIVE (ADD COLUMN) wins
// over the foreign key's own SHARE ROW EXCLUSIVE.
func TestASTTreeAddColumnInlineConstraints(t *testing.T) {
	cases := map[pg_query.ConstrType]string{
		pg_query.ConstrType_CONSTR_UNIQUE:  "ddl_add_key_builds_index",
		pg_query.ConstrType_CONSTR_PRIMARY: "ddl_add_key_builds_index",
		pg_query.ConstrType_CONSTR_FOREIGN: "ddl_fk_not_valid",
		pg_query.ConstrType_CONSTR_CHECK:   "ddl_constraint_not_valid",
	}
	for typ, id := range cases {
		def := columnDefNode(typeName("pg_catalog", "int4"), &pg_query.Constraint{Contype: typ})
		found, known := testMapper.statement(addColumnStmt(def))
		if !known {
			t.Fatalf("%v unrecognized", typ)
		}
		requireOneRule(t, found, id, lockAccessExclusive)
	}
}

func TestASTTreeAddColumnSafeAndUnknownTypes(t *testing.T) {
	safe := columnDefNode(typeName("text"),
		&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_NOTNULL},
		&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_DEFAULT,
			RawExpr: pg_query.MakeAConstStrNode("x", 0)})
	found, known := testMapper.statement(addColumnStmt(safe))
	if !known || len(found) != 0 {
		t.Fatalf("ADD COLUMN text NOT NULL DEFAULT 'x': rules %v known %v", ruleIDs(found), known)
	}
	domain := columnDefNode(typeName("app", "positive_int"))
	found, known = testMapper.statement(addColumnStmt(domain))
	if known || len(found) != 0 {
		t.Fatalf("ADD COLUMN of a user type: rules %v known %v, want unrecognized",
			ruleIDs(found), known)
	}
	serial := columnDefNode(typeName("bigserial"))
	found, known = testMapper.statement(addColumnStmt(serial))
	if !known {
		t.Fatal("bigserial unrecognized")
	}
	requireOneRule(t, found, "ddl_add_column_volatile_default", lockAccessExclusive)
}

// Nil/empty: a nil or empty node and a nil column definition are never
// known safe.
func TestASTTreeNilAndEmptyAreUnrecognized(t *testing.T) {
	for name, n := range map[string]*pg_query.Node{"nil": nil, "empty": {}} {
		if found, known := testMapper.statement(n); known || len(found) != 0 {
			t.Fatalf("%s node: rules %v known %v", name, ruleIDs(found), known)
		}
	}
	if found, known := testMapper.addColumn(nil); known || found != nil {
		t.Fatalf("nil column definition: %v %v", found, known)
	}
	found, known := testMapper.statement(alterStmt(cmd(pg_query.AlterTableType_AT_AddColumn, nil)))
	if known {
		t.Fatalf("ADD COLUMN without a definition known: %v", ruleIDs(found))
	}
}

// An unmapped subcommand makes the whole statement unrecognized, but the
// rules of its other subcommands are kept.
func TestASTTreeUnknownSubcommandIsUnrecognized(t *testing.T) {
	found, known := testMapper.statement(alterStmt(
		cmd(pg_query.AlterTableType_AT_DropColumn, nil),
		cmd(pg_query.AlterTableType_AT_SetExpression, nil)))
	if known {
		t.Fatal("SET EXPRESSION (a rewrite the mapping has no entry for) is known")
	}
	requireOneRule(t, found, "ddl_drop_column", lockAccessExclusive)
}
