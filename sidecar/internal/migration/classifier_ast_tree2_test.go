package migration

import (
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// PG 18's ADD CONSTRAINT n NOT NULL col: unrecognized before 18 (the
// server rejects it), a scan without NOT VALID, safe with it.
func TestASTTreeNotNullConstraintVersion(t *testing.T) {
	nn := func(notValid bool) *pg_query.Node {
		return alterStmt(cmd(pg_query.AlterTableType_AT_AddConstraint, constraintNode(
			&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_NOTNULL,
				SkipValidation: notValid, Keys: []*pg_query.Node{strNode("c")}})))
	}
	if found, known := (astMapper{stmt: "stmt", major: 17}).statement(nn(false)); known {
		t.Fatalf("PG 17 NOT NULL constraint known: %v", ruleIDs(found))
	}
	for _, major := range []int{18, 0} {
		m := astMapper{stmt: "stmt", major: major}
		found, known := m.statement(nn(false))
		if !known {
			t.Fatalf("PG %d: NOT NULL constraint unrecognized", major)
		}
		c := requireOneRule(t, found, "ddl_not_null_constraint_scan", lockAccessExclusive)
		if c.ColumnName != "c" {
			t.Fatalf("column %q, want c", c.ColumnName)
		}
		if found, known := m.statement(nn(true)); !known || len(found) != 0 {
			t.Fatalf("PG %d NOT VALID: rules %v known %v, want safe", major, ruleIDs(found), known)
		}
	}
}

func TestASTTreeAddConstraintForms(t *testing.T) {
	add := func(c *pg_query.Constraint) ([]DDLClassification, bool) {
		return testMapper.statement(alterStmt(cmd(pg_query.AlterTableType_AT_AddConstraint,
			constraintNode(c))))
	}
	found, _ := add(&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_EXCLUSION})
	requireOneRule(t, found, "ddl_add_exclusion_builds_index", lockAccessExclusive)
	found, _ = add(&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_FOREIGN})
	requireOneRule(t, found, "ddl_fk_not_valid", lockShareRowExclusive)
	for name, c := range map[string]*pg_query.Constraint{
		"FK NOT VALID":       {Contype: pg_query.ConstrType_CONSTR_FOREIGN, SkipValidation: true},
		"CHECK NOT VALID":    {Contype: pg_query.ConstrType_CONSTR_CHECK, SkipValidation: true},
		"UNIQUE USING INDEX": {Contype: pg_query.ConstrType_CONSTR_UNIQUE, Indexname: "u_idx"},
	} {
		if found, known := add(c); !known || len(found) != 0 {
			t.Fatalf("%s: rules %v known %v, want safe", name, ruleIDs(found), known)
		}
	}
	if _, known := add(&pg_query.Constraint{Contype: pg_query.ConstrType_CONSTR_DEFAULT}); known {
		t.Fatal("ADD CONSTRAINT of an unexpected kind is known")
	}
}

func TestASTTreeRename(t *testing.T) {
	rename := func(typ pg_query.ObjectType, rv *pg_query.RangeVar) *pg_query.Node {
		return &pg_query.Node{Node: &pg_query.Node_RenameStmt{RenameStmt: &pg_query.RenameStmt{
			RenameType: typ, Relation: rv, Subname: "a", Newname: "b"}}}
	}
	found, _ := testMapper.statement(
		rename(pg_query.ObjectType_OBJECT_COLUMN, rangeVar("app", "t")))
	if c := requireOneRule(t, found, "ddl_rename", lockAccessExclusive); c.ColumnName != "a" {
		t.Fatalf("column %q, want a", c.ColumnName)
	}
	found, known := testMapper.statement(
		rename(pg_query.ObjectType_OBJECT_INDEX, rangeVar("app", "i")))
	if !known || len(found) != 1 || found[0].LockLevel != lockShareUpdateExclusive ||
		found[0].TableName != "" {
		t.Fatalf("index rename: %+v known %v, want SHARE UPDATE EXCLUSIVE, no table", found, known)
	}
	if found, known := testMapper.statement(rename(pg_query.ObjectType_OBJECT_SCHEMA, nil)); known {
		t.Fatalf("schema rename known: %v", ruleIDs(found))
	}
}

func TestASTTreeTriggerDropAndMaintenance(t *testing.T) {
	trig := &pg_query.Node{Node: &pg_query.Node_CreateTrigStmt{
		CreateTrigStmt: &pg_query.CreateTrigStmt{Trigname: "trg", Relation: rangeVar("app", "t")}}}
	found, _ := testMapper.statement(trig)
	requireOneRule(t, found, "ddl_create_trigger", lockShareRowExclusive)

	drop := func(concurrent bool) *pg_query.Node {
		return &pg_query.Node{Node: &pg_query.Node_DropStmt{DropStmt: &pg_query.DropStmt{
			RemoveType: pg_query.ObjectType_OBJECT_INDEX, Concurrent: concurrent,
			Objects: []*pg_query.Node{pg_query.MakeListNode([]*pg_query.Node{strNode("i")})}}}}
	}
	if found, known := testMapper.statement(drop(false)); !known || len(found) != 1 ||
		found[0].RuleID != "ddl_drop_index_not_concurrent" {
		t.Fatalf("DROP INDEX: %v known %v", ruleIDs(found), known)
	}
	if found, known := testMapper.statement(drop(true)); known {
		t.Fatalf("DROP INDEX CONCURRENTLY is proven safe: %v", ruleIDs(found))
	}
	vac := func(opts ...*pg_query.Node) *pg_query.Node {
		return &pg_query.Node{Node: &pg_query.Node_VacuumStmt{VacuumStmt: &pg_query.VacuumStmt{
			IsVacuumcmd: true, Options: opts}}}
	}
	if found, known := testMapper.statement(vac()); !known || len(found) != 0 {
		t.Fatalf("plain VACUUM: %v %v", ruleIDs(found), known)
	}
	full := pg_query.MakeSimpleDefElemNode("full", nil, 0)
	found, _ = testMapper.statement(vac(full))
	if len(found) != 1 || found[0].RuleID != "ddl_vacuum_full" {
		t.Fatalf("VACUUM (FULL): %v", ruleIDs(found))
	}
	off := pg_query.MakeSimpleDefElemNode("full", strNode("false"), 0)
	if found, _ := testMapper.statement(vac(off)); len(found) != 0 {
		t.Fatalf("VACUUM (FULL false) flagged: %v", ruleIDs(found))
	}
}

func TestASTTreeHelpers(t *testing.T) {
	vc := typeName("pg_catalog", "varchar")
	vc.Typmods = []*pg_query.Node{pg_query.MakeAConstIntNode(20, 0)}
	if got := typeText(vc); got != "varchar(20)" {
		t.Fatalf("typeText = %q, want varchar(20)", got)
	}
	if got := typeText(nil); got != "" {
		t.Fatalf("typeText(nil) = %q", got)
	}
	call := func(names ...string) *pg_query.Node {
		var ns []*pg_query.Node
		for _, n := range names {
			ns = append(ns, strNode(n))
		}
		return pg_query.MakeFuncCallNode(ns, nil, 0)
	}
	for _, tc := range []struct {
		expr *pg_query.Node
		want bool
	}{
		{call("now"), false}, {call("pg_catalog", "now"), false}, {call("app", "now"), true},
		{call("gen_random_uuid"), true}, {pg_query.MakeAConstStrNode("x", 0), false}, {nil, false},
	} {
		if got := volatileExpr(tc.expr); got != tc.want {
			t.Fatalf("volatileExpr(%v) = %v, want %v", tc.expr, got, tc.want)
		}
	}
	if strongerLock("SHARE", lockAccessExclusive) != lockAccessExclusive ||
		strongerLock(lockAccessExclusive, "") != lockAccessExclusive ||
		strongerLock("", lockShareRowExclusive) != lockShareRowExclusive {
		t.Fatal("strongerLock does not keep the stronger level")
	}
	for v, want := range map[int]int{0: 0, 14: 14, 140000: 14, 180002: 18, -1: 0, 999: 0} {
		if got := serverMajor(v); got != want {
			t.Fatalf("serverMajor(%d) = %d, want %d", v, got, want)
		}
	}
}

// The PG 18 NOT NULL constraint form, read from text when the parser
// (PG 17 grammar) rejects it.
func TestNotNullConstraintText(t *testing.T) {
	stmt := `ALTER TABLE app."T" ADD CONSTRAINT c_nn NOT NULL "Col"`
	found, ok := notNullConstraintText(stmt, 18)
	if !ok || len(found) != 1 || found[0].RuleID != "ddl_not_null_constraint_scan" ||
		found[0].SchemaName != "app" || found[0].TableName != "T" || found[0].ColumnName != "Col" {
		t.Fatalf("PG 18: %+v ok %v", found, ok)
	}
	if found, ok := notNullConstraintText(stmt+" NOT VALID", 18); !ok || len(found) != 0 {
		t.Fatalf("NOT VALID: %+v ok %v, want known safe", found, ok)
	}
	if _, ok := notNullConstraintText(stmt, 17); ok {
		t.Fatal("PG 17 accepts the NOT NULL constraint form")
	}
	if _, ok := notNullConstraintText("ALTER TABLE t ADD COLUMN c int NOT NULL", 18); ok {
		t.Fatal("ADD COLUMN ... NOT NULL read as a NOT NULL constraint")
	}
}

// ALTER COLUMN ... TYPE names its target type for refineAlterType, except
// with USING (the ColumnDef's raw default), which can compute any value:
// that change stays a rewrite, so no target type is given to clear it.
func TestASTTreeAlterTypeUsingKeepsRewrite(t *testing.T) {
	alter := func(using *pg_query.Node) DDLClassification {
		def := &pg_query.ColumnDef{TypeName: typeName("text"), RawDefault: using}
		c := cmd(pg_query.AlterTableType_AT_AlterColumnType,
			&pg_query.Node{Node: &pg_query.Node_ColumnDef{ColumnDef: def}})
		c.Name = "v"
		found, known := testMapper.statement(alterStmt(c))
		if !known {
			t.Fatal("ALTER COLUMN TYPE is unrecognized")
		}
		return requireOneRule(t, found, "ddl_alter_type_rewrite", lockAccessExclusive)
	}
	if c := alter(nil); c.TargetType != "text" || c.ColumnName != "v" || !c.RequiresRewrite {
		t.Fatalf("plain TYPE text: %+v", c)
	}
	lower := &pg_query.Node{Node: &pg_query.Node_FuncCall{FuncCall: &pg_query.FuncCall{
		Funcname: []*pg_query.Node{strNode("lower")}}}}
	if c := alter(lower); c.TargetType != "" || !c.RequiresRewrite {
		t.Fatalf("TYPE text USING lower(v): %+v, want no target type", c)
	}
}

// A COLLATE clause, or a second type change in the same statement, also
// hides a rewrite from the one target type a classification carries: a
// new collation rebuilds the column's indexes, and one statement's two
// type changes share one classification. Neither names a target type,
// so refineAlterType cannot clear the rewrite (migration-04 review).
func TestASTTreeAlterTypeHiddenRewritesNameNoTarget(t *testing.T) {
	typeCmd := func(col, typ string, coll *pg_query.CollateClause) *pg_query.AlterTableCmd {
		def := &pg_query.ColumnDef{TypeName: typeName(typ), CollClause: coll}
		c := cmd(pg_query.AlterTableType_AT_AlterColumnType,
			&pg_query.Node{Node: &pg_query.Node_ColumnDef{ColumnDef: def}})
		c.Name = col
		return c
	}
	collateC := &pg_query.CollateClause{Collname: []*pg_query.Node{strNode("C")}}
	for name, stmt := range map[string]*pg_query.Node{
		"COLLATE":          alterStmt(typeCmd("v", "text", collateC)),
		"two type changes": alterStmt(typeCmd("v", "text", nil), typeCmd("n", "int8", nil)),
	} {
		found, known := testMapper.statement(stmt)
		if !known {
			t.Fatalf("%s: unrecognized", name)
		}
		c := requireOneRule(t, found, "ddl_alter_type_rewrite", lockAccessExclusive)
		if c.TargetType != "" || !c.RequiresRewrite || c.ColumnName != "v" {
			t.Fatalf("%s: %+v, want a rewrite of v with no target type", name, c)
		}
	}
	// Boundary: one type change next to another subcommand keeps its target.
	found, _ := testMapper.statement(alterStmt(typeCmd("v", "text", nil),
		cmd(pg_query.AlterTableType_AT_ColumnDefault, nil)))
	c := requireOneRule(t, found, "ddl_alter_type_rewrite", lockAccessExclusive)
	if c.TargetType != "text" {
		t.Fatalf("TYPE text, SET DEFAULT: %+v, want target text", c)
	}
}
