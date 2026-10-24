//go:build cgo

package sqlast

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// relationObjectTypes are the relation kinds drop_table and rename_table
// cover.
var relationObjectTypes = map[pg_query.ObjectType]string{
	pg_query.ObjectType_OBJECT_TABLE:         "table",
	pg_query.ObjectType_OBJECT_VIEW:          "view",
	pg_query.ObjectType_OBJECT_MATVIEW:       "materialized_view",
	pg_query.ObjectType_OBJECT_FOREIGN_TABLE: "foreign_table",
}

func behaviorName(b pg_query.DropBehavior) string {
	if b == pg_query.DropBehavior_DROP_CASCADE {
		return "cascade"
	}
	return "restrict"
}

// dropStatements yields one entry per dropped index, table or view.
func dropStatements(drop *pg_query.DropStmt) []DDLStatement {
	objectType, isRelation := relationObjectTypes[drop.GetRemoveType()]
	isIndex := drop.GetRemoveType() == pg_query.ObjectType_OBJECT_INDEX
	if !isRelation && !isIndex {
		return otherDrop(drop)
	}
	out := make([]DDLStatement, 0, len(drop.GetObjects()))
	for _, object := range drop.GetObjects() {
		name := nameFromList(object.GetList().GetItems())
		entry := DDLStatement{Behavior: behaviorName(drop.GetBehavior()),
			IfExists: drop.GetMissingOk()}
		if isIndex {
			entry.Kind, entry.Object = DDLDropIndex, name
			entry.Index = &IndexDef{Name: name.Name, Concurrent: drop.GetConcurrent()}
		} else {
			entry.Kind, entry.Relation, entry.ObjectType = DDLDropTable, name, objectType
		}
		out = append(out, entry)
	}
	return out
}

func renameStatement(rename *pg_query.RenameStmt) DDLStatement {
	relation := rangeVarName(rename.GetRelation())
	if rename.GetRenameType() == pg_query.ObjectType_OBJECT_COLUMN {
		return DDLStatement{Kind: DDLRenameColumn, Relation: relation,
			Columns: []string{rename.GetSubname()}, NewName: rename.GetNewname(),
			IfExists: rename.GetMissingOk()}
	}
	if objectType, ok := relationObjectTypes[rename.GetRenameType()]; ok {
		return DDLStatement{Kind: DDLRenameTable, Relation: relation, ObjectType: objectType,
			NewName: rename.GetNewname(), IfExists: rename.GetMissingOk()}
	}
	return DDLStatement{Kind: DDLOther}
}

// alterTable yields one entry per ALTER TABLE subcommand. ALTER INDEX,
// VIEW, SEQUENCE and the like are "other".
func alterTable(alter *pg_query.AlterTableStmt) []DDLStatement {
	if alter.GetObjtype() != pg_query.ObjectType_OBJECT_TABLE {
		return []DDLStatement{{Kind: DDLOther}}
	}
	relation := rangeVarName(alter.GetRelation())
	out := make([]DDLStatement, 0, len(alter.GetCmds()))
	for _, node := range alter.GetCmds() {
		cmd := node.GetAlterTableCmd()
		entry := alterCommand(cmd)
		entry.Relation = relation
		entry.Subcommand = cmd.GetSubtype().String()
		entry.IfExists = entry.IfExists || alter.GetMissingOk()
		out = append(out, entry)
	}
	return out
}

func alterCommand(cmd *pg_query.AlterTableCmd) DDLStatement {
	name := cmd.GetName()
	column := func(kind DDLKind) DDLStatement {
		return DDLStatement{Kind: kind, Columns: []string{name}, Column: &ColumnDef{Name: name}}
	}
	switch cmd.GetSubtype() {
	case pg_query.AlterTableType_AT_AddColumn:
		def := columnDef(cmd.GetDef().GetColumnDef())
		return DDLStatement{Kind: DDLAddColumn, Columns: []string{def.Name}, Column: &def,
			IfNotExists: cmd.GetMissingOk()}
	case pg_query.AlterTableType_AT_DropColumn:
		entry := column(DDLDropColumn)
		entry.Behavior, entry.IfExists = behaviorName(cmd.GetBehavior()), cmd.GetMissingOk()
		return entry
	case pg_query.AlterTableType_AT_AlterColumnType:
		entry := column(DDLAlterType)
		def := cmd.GetDef().GetColumnDef()
		entry.Column.Type = typeName(def.GetTypeName())
		entry.Column.Using = deparseExpr(def.GetRawDefault())
		return entry
	case pg_query.AlterTableType_AT_SetNotNull:
		return column(DDLSetNotNull)
	case pg_query.AlterTableType_AT_DropNotNull:
		return column(DDLDropNotNull)
	case pg_query.AlterTableType_AT_ColumnDefault:
		entry := column(DDLSetDefault)
		if def := cmd.GetDef(); def != nil {
			entry.Column.HasDefault = true
			entry.Column.Default = deparseExpr(def)
			entry.Column.DefaultVolatile = mayBeVolatile(def)
		}
		return entry
	}
	return constraintCommand(cmd)
}

func constraintCommand(cmd *pg_query.AlterTableCmd) DDLStatement {
	switch cmd.GetSubtype() {
	case pg_query.AlterTableType_AT_AddConstraint:
		def := constraintDef(cmd.GetDef().GetConstraint(), "")
		return DDLStatement{Kind: DDLAddConstraint, Columns: def.Columns, Constraint: &def}
	case pg_query.AlterTableType_AT_ValidateConstraint:
		return DDLStatement{Kind: DDLValidateConstraint,
			Constraint: &ConstraintDef{Name: cmd.GetName()}}
	case pg_query.AlterTableType_AT_DropConstraint:
		return DDLStatement{Kind: DDLDropConstraint,
			Constraint: &ConstraintDef{Name: cmd.GetName()},
			Behavior:   behaviorName(cmd.GetBehavior()), IfExists: cmd.GetMissingOk()}
	}
	return DDLStatement{Kind: DDLOther}
}
