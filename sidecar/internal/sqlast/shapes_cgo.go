//go:build cgo

package sqlast

import (
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// safeAlterTableCmds are the single ALTER TABLE subcommands the executor
// runs: storage parameters and the rehearsed online-migration steps.
var safeAlterTableCmds = map[pg_query.AlterTableType]bool{
	pg_query.AlterTableType_AT_SetRelOptions:      true,
	pg_query.AlterTableType_AT_ResetRelOptions:    true,
	pg_query.AlterTableType_AT_AddConstraint:      true,
	pg_query.AlterTableType_AT_ValidateConstraint: true,
	pg_query.AlterTableType_AT_SetNotNull:         true,
	pg_query.AlterTableType_AT_DropConstraint:     true,
}

func checkAlterTable(alter *pg_query.AlterTableStmt, rules Rules) error {
	if len(alter.GetCmds()) != 1 {
		return reject("ALTER TABLE must carry exactly one subcommand")
	}
	cmd := alter.GetCmds()[0].GetAlterTableCmd()
	if cmd == nil || !safeAlterTableCmds[cmd.GetSubtype()] {
		return reject("ALTER TABLE subcommand is not allowed")
	}
	if cmd.GetSubtype() == pg_query.AlterTableType_AT_DropConstraint &&
		cmd.GetBehavior() == pg_query.DropBehavior_DROP_CASCADE {
		return reject("ALTER TABLE ... DROP CONSTRAINT CASCADE is not allowed")
	}
	if cmd.GetSubtype() == pg_query.AlterTableType_AT_AddConstraint {
		return checkAddedConstraint(cmd.GetDef().GetConstraint())
	}
	if cmd.GetSubtype() == pg_query.AlterTableType_AT_SetRelOptions ||
		cmd.GetSubtype() == pg_query.AlterTableType_AT_ResetRelOptions {
		reset := cmd.GetSubtype() == pg_query.AlterTableType_AT_ResetRelOptions
		return checkReloptions(cmd.GetDef().GetList().GetItems(), reset, rules)
	}
	return nil
}

// checkReloptions applies the reloption rule to each storage parameter as
// the parser resolved it (quoted and Unicode-escaped names included).
func checkReloptions(items []*pg_query.Node, reset bool, rules Rules) error {
	if rules.Reloption == nil {
		return nil
	}
	for _, item := range items {
		def := item.GetDefElem()
		if def == nil {
			return reject("ALTER TABLE storage parameter is malformed")
		}
		key := strings.ToLower(def.GetDefname())
		if ns := def.GetDefnamespace(); ns != "" {
			key = strings.ToLower(ns) + "." + key
		}
		value := ""
		if !reset {
			value = defElemValue(def.GetArg())
		}
		if !rules.Reloption(key, value, reset) {
			return reject("storage parameter %s=%s is not allowed", key, value)
		}
	}
	return nil
}

// defElemValue renders a reloption argument; a bare option means true.
func defElemValue(arg *pg_query.Node) string {
	switch {
	case arg == nil:
		return "true"
	case arg.GetString_() != nil:
		return arg.GetString_().GetSval()
	case arg.GetInteger() != nil:
		return strconv.Itoa(int(arg.GetInteger().GetIval()))
	case arg.GetFloat() != nil:
		return arg.GetFloat().GetFval()
	case arg.GetBoolean() != nil:
		return strconv.FormatBool(arg.GetBoolean().GetBoolval())
	case arg.GetTypeName() != nil:
		return typeNameValue(arg.GetTypeName())
	}
	return ""
}

// typeNameValue renders an unquoted keyword the grammar parsed as a type
// name (e.g. "on" in some reloption positions).
func typeNameValue(name *pg_query.TypeName) string {
	parts := make([]string, 0, len(name.GetNames()))
	for _, part := range name.GetNames() {
		parts = append(parts, part.GetString_().GetSval())
	}
	return strings.Join(parts, ".")
}

// checkAddedConstraint allows only the online-migration forms:
// CHECK (col IS NOT NULL) NOT VALID, and UNIQUE USING INDEX.
func checkAddedConstraint(constraint *pg_query.Constraint) error {
	switch {
	case constraint == nil:
		return reject("ALTER TABLE ADD needs a constraint")
	case constraint.GetContype() == pg_query.ConstrType_CONSTR_UNIQUE &&
		constraint.GetIndexname() != "":
		return nil
	case constraint.GetContype() == pg_query.ConstrType_CONSTR_CHECK &&
		constraint.GetSkipValidation() && isNotNullTest(constraint.GetRawExpr()):
		return nil
	default:
		return reject("ALTER TABLE ADD constraint must be CHECK (col IS NOT NULL) " +
			"NOT VALID or UNIQUE USING INDEX")
	}
}

func isNotNullTest(expr *pg_query.Node) bool {
	test := expr.GetNullTest()
	return test != nil && test.GetNulltesttype() == pg_query.NullTestType_IS_NOT_NULL &&
		test.GetArg().GetColumnRef() != nil
}

// checkBackendSignal allows only SELECT pg_cancel_backend(<int>) or
// SELECT pg_terminate_backend(<int>): one target, no FROM, WHERE or set
// operation, and a single integer literal argument.
func checkBackendSignal(sel *pg_query.SelectStmt) error {
	if sel.GetOp() != pg_query.SetOperation_SETOP_NONE || sel.GetFromClause() != nil ||
		sel.GetWhereClause() != nil || len(sel.GetTargetList()) != 1 {
		return reject("SELECT must call one backend-signal function with one literal PID")
	}
	call := sel.GetTargetList()[0].GetResTarget().GetVal().GetFuncCall()
	if call == nil || !isBackendSignalFunc(call.GetFuncname()) {
		return reject("SELECT may call only the pg_cancel_backend/pg_terminate_backend function")
	}
	if len(call.GetArgs()) != 1 || call.GetArgs()[0].GetAConst().GetIval() == nil {
		return reject("backend signal needs exactly one literal integer PID")
	}
	return nil
}

func isBackendSignalFunc(name []*pg_query.Node) bool {
	parts := make([]string, 0, len(name))
	for _, part := range name {
		parts = append(parts, part.GetString_().GetSval())
	}
	switch strings.Join(parts, ".") {
	case "pg_cancel_backend", "pg_terminate_backend",
		"pg_catalog.pg_cancel_backend", "pg_catalog.pg_terminate_backend":
		return true
	}
	return false
}

// protectedRelation walks the whole parse tree and returns the first
// protected schema named by any relation, so no clause can smuggle one in.
func protectedRelation(root *pg_query.Node, rules Rules) string {
	if rules.ProtectedSchema == nil {
		return ""
	}
	var found string
	walk(root.ProtoReflect(), func(msg protoreflect.Message) bool {
		if rv, ok := msg.Interface().(*pg_query.RangeVar); ok &&
			rules.ProtectedSchema(strings.ToLower(rv.GetSchemaname())) {
			found = rv.GetSchemaname()
		}
		return found == ""
	})
	return found
}

func walk(msg protoreflect.Message, visit func(protoreflect.Message) bool) bool {
	if !visit(msg) {
		return false
	}
	keep := true
	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Message() != nil:
			list := value.List()
			for i := 0; i < list.Len() && keep; i++ {
				keep = walk(list.Get(i).Message(), visit)
			}
		case field.Message() != nil && !field.IsMap():
			keep = walk(value.Message(), visit)
		}
		return keep
	})
	return keep
}
