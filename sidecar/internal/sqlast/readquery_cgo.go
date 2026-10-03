//go:build cgo

package sqlast

import (
	"sort"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// InspectReadQuery parses exactly one SELECT (including WITH, VALUES and
// TABLE forms) and lists what executing it could invoke.
func InspectReadQuery(sql string) (ReadQuery, error) {
	result, err := pg_query.Parse(sql)
	if err != nil {
		return ReadQuery{}, reject("parse error: %v", err)
	}
	stmts := result.GetStmts()
	if len(stmts) != 1 {
		return ReadQuery{}, reject("exactly one statement is allowed, got %d", len(stmts))
	}
	root := stmts[0].GetStmt()
	if root.GetSelectStmt() == nil {
		return ReadQuery{}, reject("only a SELECT statement can be inspected")
	}
	c := newCollector()
	walk(root.ProtoReflect(), func(msg protoreflect.Message) bool {
		c.visit(msg.Interface())
		return true
	})
	return c.result(), nil
}

type nameSet map[QualifiedName]bool

type collector struct {
	functions, attributes, operators, types, relations nameSet
	ctes                                               map[string]bool
	out                                                ReadQuery
}

func newCollector() *collector {
	return &collector{functions: nameSet{}, attributes: nameSet{}, operators: nameSet{},
		types: nameSet{}, relations: nameSet{}, ctes: map[string]bool{}}
}

func (c *collector) visit(node any) {
	switch n := node.(type) {
	case *pg_query.FuncCall:
		c.functions.add(nameFromList(n.GetFuncname()))
	case *pg_query.A_Expr:
		c.visitAExpr(n)
	case *pg_query.SubLink:
		c.visitSubLink(n)
	case *pg_query.CaseExpr:
		if n.GetArg() != nil {
			c.operators.add(QualifiedName{Name: "="})
		}
	case *pg_query.MinMaxExpr:
		c.operators.add(QualifiedName{Name: "<"})
		c.operators.add(QualifiedName{Name: ">"})
	case *pg_query.TypeCast:
		c.types.add(nameFromList(n.GetTypeName().GetNames()))
	case *pg_query.RangeVar:
		c.relations.add(QualifiedName{Schema: n.GetSchemaname(), Name: n.GetRelname()})
	case *pg_query.CommonTableExpr:
		c.ctes[n.GetCtename()] = true
	case *pg_query.ColumnRef:
		c.visitColumnRef(n.GetFields())
	case *pg_query.A_Indirection:
		c.visitIndirection(n.GetIndirection())
	case *pg_query.SelectStmt:
		c.out.LockingClause = c.out.LockingClause || len(n.GetLockingClause()) > 0
		c.out.SelectInto = c.out.SelectInto || n.GetIntoClause() != nil
	case *pg_query.InsertStmt, *pg_query.UpdateStmt, *pg_query.DeleteStmt,
		*pg_query.MergeStmt:
		c.out.ModifiesData = true
	}
}

// visitAExpr records the operators an expression kind evaluates. BETWEEN
// kinds carry the keyword as their name, so the comparison operators they
// expand to are recorded instead.
func (c *collector) visitAExpr(expr *pg_query.A_Expr) {
	switch expr.GetKind() {
	case pg_query.A_Expr_Kind_AEXPR_BETWEEN, pg_query.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		pg_query.A_Expr_Kind_AEXPR_BETWEEN_SYM, pg_query.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		for _, op := range []string{"<", "<=", ">", ">="} {
			c.operators.add(QualifiedName{Name: op})
		}
	default:
		c.operators.add(nameFromList(expr.GetName()))
	}
}

// visitSubLink records the comparison of ANY/ALL/row-compare sublinks; an
// empty operator name (x IN (SELECT ...)) means "=".
func (c *collector) visitSubLink(link *pg_query.SubLink) {
	switch link.GetSubLinkType() {
	case pg_query.SubLinkType_ANY_SUBLINK, pg_query.SubLinkType_ALL_SUBLINK,
		pg_query.SubLinkType_ROWCOMPARE_SUBLINK:
		name := nameFromList(link.GetOperName())
		if name.Name == "" {
			name = QualifiedName{Name: "="}
		}
		c.operators.add(name)
	}
}

func (c *collector) visitColumnRef(fields []*pg_query.Node) {
	if len(fields) < 2 {
		return
	}
	if last := fields[len(fields)-1].GetString_(); last != nil {
		c.attributes.add(QualifiedName{Name: last.GetSval()})
	}
}

func (c *collector) visitIndirection(items []*pg_query.Node) {
	for _, item := range items {
		if s := item.GetString_(); s != nil {
			c.attributes.add(QualifiedName{Name: s.GetSval()})
		}
	}
}

func (c *collector) result() ReadQuery {
	out := c.out
	out.Functions = c.functions.sorted()
	out.AttributeCalls = c.attributes.sorted()
	out.Operators = c.operators.sorted()
	out.Types = c.types.sorted()
	out.Relations = c.relations.sorted()
	for name := range c.ctes {
		out.CTENames = append(out.CTENames, name)
	}
	sort.Strings(out.CTENames)
	return out
}

func (s nameSet) add(name QualifiedName) {
	if name.Name != "" {
		s[name] = true
	}
}

func (s nameSet) sorted() []QualifiedName {
	out := make([]QualifiedName, 0, len(s))
	for name := range s {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// nameFromList turns a parser name list into schema and object name. A
// three-part name is catalog.schema.name; the catalog must be the current
// database, so only schema and name matter.
func nameFromList(list []*pg_query.Node) QualifiedName {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		if s := item.GetString_(); s != nil {
			parts = append(parts, s.GetSval())
		}
	}
	switch len(parts) {
	case 0:
		return QualifiedName{}
	case 1:
		return QualifiedName{Name: parts[0]}
	default:
		return QualifiedName{Schema: parts[len(parts)-2], Name: parts[len(parts)-1]}
	}
}
