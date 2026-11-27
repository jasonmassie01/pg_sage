//go:build cgo

package sqlast

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// InspectStatements parses a script of one or more statements and
// describes each one. A parse error is wrapped in ErrRejected.
func InspectStatements(sql string) ([]Statement, error) {
	result, err := pg_query.Parse(sql)
	if err != nil {
		return nil, reject("parse error: %v", err)
	}
	out := make([]Statement, 0, len(result.GetStmts()))
	for _, raw := range result.GetStmts() {
		out = append(out, describeStatement(raw.GetStmt()))
	}
	return out, nil
}

func describeStatement(root *pg_query.Node) Statement {
	st := Statement{Kind: KindOther, Node: nodeName(root)}
	classifyStatement(root, &st)
	ctes := scopedCTENames(root)
	relations, functions, attributes := nameSet{}, nameSet{}, nameSet{}
	walk(root.ProtoReflect(), func(msg protoreflect.Message) bool {
		switch n := msg.Interface().(type) {
		case *pg_query.RangeVar:
			relations.add(QualifiedName{Schema: n.GetSchemaname(), Name: n.GetRelname()})
		case *pg_query.FuncCall:
			functions.add(nameFromList(n.GetFuncname()))
		case *pg_query.ColumnRef, *pg_query.A_Indirection:
			addAttributeCalls(n, attributes)
		case *pg_query.InsertStmt, *pg_query.DeleteStmt, *pg_query.MergeStmt:
			st.NestedWrite = true
		case *pg_query.UpdateStmt:
			st.NestedWrite = st.NestedWrite || n != root.GetUpdateStmt()
		}
		return true
	})
	for name := range relations {
		if name.Schema == "" && ctes[name.Name] {
			delete(relations, name)
		}
	}
	if target := root.GetUpdateStmt().GetRelation(); target != nil {
		// The UPDATE target is always a table, never a CTE of that name.
		relations.add(QualifiedName{Schema: target.GetSchemaname(), Name: target.GetRelname()})
	}
	st.Relations, st.Functions = relations.sorted(), functions.sorted()
	st.AttributeCalls = attributes.sorted()
	return st
}

// scopedCTENames are the CTE names of the statement's own top-level WITH,
// which are in scope everywhere a reference to them can be written. A
// nested WITH has a narrower scope, so its names are not collected and a
// reference to them counts as a relation (fail closed). When a CTE body
// names itself or a later CTE, which there is a table, none are collected.
func scopedCTENames(root *pg_query.Node) map[string]bool {
	var with *pg_query.WithClause
	switch {
	case root.GetSelectStmt() != nil:
		with = root.GetSelectStmt().GetWithClause()
	case root.GetUpdateStmt() != nil:
		with = root.GetUpdateStmt().GetWithClause()
	}
	ctes := with.GetCtes()
	names := make([]string, len(ctes))
	for i, cte := range ctes {
		names[i] = cte.GetCommonTableExpr().GetCtename()
	}
	for i, cte := range ctes {
		if referencesAny(cte.GetCommonTableExpr().GetCtequery(), names[i:]) {
			return nil
		}
	}
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// referencesAny reports whether node names an unqualified relation in names.
func referencesAny(node *pg_query.Node, names []string) bool {
	if node == nil {
		return false
	}
	found := false
	walk(node.ProtoReflect(), func(msg protoreflect.Message) bool {
		if rv, ok := msg.Interface().(*pg_query.RangeVar); ok && rv.GetSchemaname() == "" {
			for _, name := range names {
				found = found || rv.GetRelname() == name
			}
		}
		return !found
	})
	return found
}

// classifyStatement fills the kind and the kind-specific fields.
func classifyStatement(root *pg_query.Node, st *Statement) {
	switch {
	case root.GetAlterTableStmt() != nil:
		alter := root.GetAlterTableStmt()
		st.Kind = KindAlterTable
		st.TableTarget = alter.GetObjtype() == pg_query.ObjectType_OBJECT_TABLE
		for _, cmd := range alter.GetCmds() {
			st.Commands = append(st.Commands, cmd.GetAlterTableCmd().GetSubtype().String())
		}
	case root.GetRenameStmt() != nil:
		rename := root.GetRenameStmt()
		st.Kind = KindRename
		st.TableTarget = rename.GetRelationType() == pg_query.ObjectType_OBJECT_TABLE
		st.Commands = []string{rename.GetRenameType().String()}
	case root.GetIndexStmt() != nil:
		st.Kind = KindCreateIndex
		st.Concurrent = root.GetIndexStmt().GetConcurrent()
	case root.GetUpdateStmt() != nil:
		st.Kind = KindUpdate
	case root.GetVacuumStmt() != nil && !root.GetVacuumStmt().GetIsVacuumcmd():
		st.Kind = KindAnalyze
	case root.GetVariableSetStmt() != nil:
		classifySetting(root.GetVariableSetStmt(), st)
	case root.GetTransactionStmt() != nil:
		switch root.GetTransactionStmt().GetKind() {
		case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN,
			pg_query.TransactionStmtKind_TRANS_STMT_START:
			st.Kind = KindBegin
		case pg_query.TransactionStmtKind_TRANS_STMT_COMMIT:
			st.Kind = KindCommit
		}
	}
}

func classifySetting(set *pg_query.VariableSetStmt, st *Statement) {
	switch set.GetKind() {
	case pg_query.VariableSetKind_VAR_SET_VALUE, pg_query.VariableSetKind_VAR_SET_DEFAULT:
		st.Kind, st.Setting = KindSet, set.GetName()
	case pg_query.VariableSetKind_VAR_RESET:
		st.Kind, st.Setting = KindReset, set.GetName()
	case pg_query.VariableSetKind_VAR_RESET_ALL:
		st.Kind = KindReset
	}
}

// nodeName is the parse-node message name of a statement, e.g. "DropStmt".
func nodeName(root *pg_query.Node) string {
	msg := root.ProtoReflect()
	field := msg.WhichOneof(msg.Descriptor().Oneofs().Get(0))
	if field == nil || field.Message() == nil {
		return ""
	}
	return string(field.Message().Name())
}
