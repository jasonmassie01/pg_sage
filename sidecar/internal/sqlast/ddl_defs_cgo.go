//go:build cgo

package sqlast

import (
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func createIndex(idx *pg_query.IndexStmt) DDLStatement {
	relation := rangeVarName(idx.GetRelation())
	def := &IndexDef{Name: idx.GetIdxname(), Unique: idx.GetUnique(),
		Concurrent: idx.GetConcurrent(), IfNotExists: idx.GetIfNotExists(),
		Method: idx.GetAccessMethod(), Predicate: deparseExpr(idx.GetWhereClause())}
	var columns []string
	for _, param := range idx.GetIndexParams() {
		elem := param.GetIndexElem()
		if elem.GetName() == "" {
			def.Columns = append(def.Columns, deparseExpr(elem.GetExpr()))
			continue
		}
		def.Columns = append(def.Columns, elem.GetName())
		columns = append(columns, elem.GetName())
	}
	for _, param := range idx.GetIndexIncludingParams() {
		if name := param.GetIndexElem().GetName(); name != "" {
			def.Include = append(def.Include, name)
			columns = append(columns, name)
		}
	}
	out := DDLStatement{Kind: DDLCreateIndex, Relation: relation, Columns: columns,
		Index: def, IfNotExists: idx.GetIfNotExists()}
	if def.Name != "" {
		out.Object = QualifiedName{Schema: relation.Schema, Name: def.Name}
	}
	return out
}

func createTable(create *pg_query.CreateStmt) DDLStatement {
	rv := create.GetRelation()
	out := DDLStatement{Kind: DDLCreateTable, Relation: rangeVarName(rv),
		IfNotExists: create.GetIfNotExists()}
	table := &TableDef{Unlogged: rv.GetRelpersistence() == "u",
		Temporary: rv.GetRelpersistence() == "t"}
	for _, elt := range create.GetTableElts() {
		switch {
		case elt.GetColumnDef() != nil:
			column := columnDef(elt.GetColumnDef())
			table.Columns = append(table.Columns, column)
			table.Constraints = append(table.Constraints, column.Constraints...)
			out.Columns = append(out.Columns, column.Name)
		case elt.GetConstraint() != nil:
			table.Constraints = append(table.Constraints,
				constraintDef(elt.GetConstraint(), ""))
		}
	}
	if spec := create.GetPartspec(); spec != nil {
		table.PartitionBy = strings.ToLower(
			strings.TrimPrefix(spec.GetStrategy().String(), "PARTITION_STRATEGY_"))
	}
	var parents []QualifiedName
	for _, parent := range create.GetInhRelations() {
		parents = append(parents, rangeVarName(parent.GetRangeVar()))
	}
	if create.GetPartbound() != nil && len(parents) == 1 {
		table.PartitionOf = &parents[0]
	} else {
		table.Inherits = parents
	}
	out.Table = table
	return out
}

// serialTypes imply NOT NULL DEFAULT nextval(...).
var serialTypes = map[string]bool{
	"serial": true, "serial4": true, "bigserial": true, "serial8": true,
	"smallserial": true, "serial2": true,
}

func columnDef(def *pg_query.ColumnDef) ColumnDef {
	out := ColumnDef{Name: def.GetColname(), Type: typeName(def.GetTypeName())}
	if serialTypes[out.Type] {
		out.NotNull, out.HasDefault, out.DefaultVolatile = true, true, true
	}
	for _, node := range def.GetConstraints() {
		c := node.GetConstraint()
		switch c.GetContype() {
		case pg_query.ConstrType_CONSTR_NOTNULL:
			out.NotNull = true
		case pg_query.ConstrType_CONSTR_DEFAULT:
			out.HasDefault = true
			out.Default = deparseExpr(c.GetRawExpr())
			out.DefaultVolatile = out.DefaultVolatile || mayBeVolatile(c.GetRawExpr())
		case pg_query.ConstrType_CONSTR_IDENTITY:
			out.Generated = GeneratedIdentityAlways
			if c.GetGeneratedWhen() == "d" {
				out.Generated = GeneratedIdentityByDefault
			}
			out.NotNull, out.HasDefault, out.DefaultVolatile = true, true, true
		case pg_query.ConstrType_CONSTR_GENERATED:
			out.Generated = GeneratedStored
			out.HasDefault, out.DefaultVolatile = true, true
		case pg_query.ConstrType_CONSTR_PRIMARY:
			out.NotNull = true
			out.Constraints = append(out.Constraints, constraintDef(c, out.Name))
		case pg_query.ConstrType_CONSTR_UNIQUE, pg_query.ConstrType_CONSTR_FOREIGN,
			pg_query.ConstrType_CONSTR_CHECK, pg_query.ConstrType_CONSTR_EXCLUSION:
			out.Constraints = append(out.Constraints, constraintDef(c, out.Name))
		}
	}
	return out
}

var constraintKinds = map[pg_query.ConstrType]string{
	pg_query.ConstrType_CONSTR_PRIMARY:   ConstraintPrimaryKey,
	pg_query.ConstrType_CONSTR_UNIQUE:    ConstraintUnique,
	pg_query.ConstrType_CONSTR_FOREIGN:   ConstraintForeignKey,
	pg_query.ConstrType_CONSTR_CHECK:     ConstraintCheck,
	pg_query.ConstrType_CONSTR_EXCLUSION: ConstraintExclusion,
}

// constraintDef describes a table constraint, or an inline constraint of
// column (whose column lists are implied).
func constraintDef(c *pg_query.Constraint, column string) ConstraintDef {
	out := ConstraintDef{Kind: constraintKinds[c.GetContype()], Name: c.GetConname(),
		NotValid: c.GetSkipValidation(), UsingIndex: c.GetIndexname()}
	if out.Kind == "" {
		out.Kind = ConstraintOther
	}
	switch c.GetContype() {
	case pg_query.ConstrType_CONSTR_PRIMARY, pg_query.ConstrType_CONSTR_UNIQUE:
		out.Columns = stringList(c.GetKeys())
	case pg_query.ConstrType_CONSTR_FOREIGN:
		out.Columns = stringList(c.GetFkAttrs())
		ref := rangeVarName(c.GetPktable())
		out.RefRelation, out.RefColumns = &ref, stringList(c.GetPkAttrs())
	case pg_query.ConstrType_CONSTR_CHECK:
		out.Expr = deparseExpr(c.GetRawExpr())
		out.Columns = exprColumns(c.GetRawExpr())
	case pg_query.ConstrType_CONSTR_EXCLUSION:
		for _, pair := range c.GetExclusions() {
			if items := pair.GetList().GetItems(); len(items) > 0 {
				if name := items[0].GetIndexElem().GetName(); name != "" {
					out.Columns = append(out.Columns, name)
				}
			}
		}
	}
	if out.Columns == nil && column != "" && c.GetContype() != pg_query.ConstrType_CONSTR_CHECK {
		out.Columns = []string{column}
	}
	return out
}

// exprColumns lists the column names an expression references, sorted.
func exprColumns(expr *pg_query.Node) []string {
	if expr == nil {
		return nil
	}
	seen := map[string]bool{}
	walk(expr.ProtoReflect(), func(msg protoreflect.Message) bool {
		if ref, ok := msg.Interface().(*pg_query.ColumnRef); ok {
			if fields := ref.GetFields(); len(fields) > 0 {
				if s := fields[len(fields)-1].GetString_(); s != nil {
					seen[s.GetSval()] = true
				}
			}
		}
		return true
	})
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// typeName renders a type as written, dropping the pg_catalog qualifier
// the grammar adds to SQL-standard types.
func typeName(name *pg_query.TypeName) string {
	if name == nil {
		return ""
	}
	parts := stringList(name.GetNames())
	if len(parts) > 1 && parts[0] == "pg_catalog" {
		parts = parts[1:]
	}
	out := strings.Join(parts, ".")
	if mods := name.GetTypmods(); len(mods) > 0 {
		rendered := make([]string, 0, len(mods))
		for _, mod := range mods {
			rendered = append(rendered, constText(mod))
		}
		out += "(" + strings.Join(rendered, ",") + ")"
	}
	for range name.GetArrayBounds() {
		out += "[]"
	}
	return out
}

// nonVolatileFuncs are built-in functions that are stable or immutable, so a
// default calling only these is evaluated once by ADD COLUMN.
var nonVolatileFuncs = map[string]bool{
	"now": true, "transaction_timestamp": true, "statement_timestamp": true,
	"current_setting": true, "lower": true, "upper": true, "btrim": true,
	"length": true, "char_length": true, "to_char": true, "to_jsonb": true,
	"jsonb_build_object": true, "json_build_object": true, "jsonb_build_array": true,
	"date_trunc": true, "make_interval": true, "concat": true, "md5": true,
	"abs": true, "round": true, "timezone": true, "substr": true, "replace": true,
}

// mayBeVolatile reports whether expr calls a function not known to be
// stable or immutable. Unknown functions count as volatile.
func mayBeVolatile(expr *pg_query.Node) bool {
	if expr == nil {
		return false
	}
	volatile := false
	walk(expr.ProtoReflect(), func(msg protoreflect.Message) bool {
		if call, ok := msg.Interface().(*pg_query.FuncCall); ok {
			name := nameFromList(call.GetFuncname())
			known := name.Schema == "" || name.Schema == "pg_catalog"
			volatile = !known || !nonVolatileFuncs[strings.ToLower(name.Name)]
		}
		return !volatile
	})
	return volatile
}
