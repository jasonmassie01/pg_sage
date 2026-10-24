//go:build cgo

package sqlast

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// analyzeSelect records one SELECT (or set operation, or VALUES) and
// returns its output columns.
func (c *refCollector) analyzeSelect(sel *pg_query.SelectStmt, parent *scope) queryOutput {
	sc := newScope(parent)
	c.addCTEs(sel.GetWithClause(), sc)
	if sel.GetOp() != pg_query.SetOperation_SETOP_NONE {
		out := c.analyzeSelect(sel.GetLarg(), sc)
		c.analyzeSelect(sel.GetRarg(), sc)
		c.walkList(sel.GetSortClause(), sc, outputNames(out))
		c.walkList([]*pg_query.Node{sel.GetLimitCount(), sel.GetLimitOffset()}, sc, nil)
		return out
	}
	if lists := sel.GetValuesLists(); len(lists) > 0 {
		return c.analyzeValues(lists, sc)
	}
	for _, item := range sel.GetFromClause() {
		c.addFromItem(item, sc)
	}
	out := c.analyzeTargets(sel.GetTargetList(), sc)
	c.walkList([]*pg_query.Node{sel.GetWhereClause(), sel.GetHavingClause(),
		sel.GetLimitCount(), sel.GetLimitOffset()}, sc, nil)
	c.walkList(sel.GetGroupClause(), sc, nil)
	c.walkList(sel.GetDistinctClause(), sc, nil)
	c.walkList(sel.GetWindowClause(), sc, nil)
	c.walkList(sel.GetSortClause(), sc, outputNames(out))
	c.applyLocking(sel.GetLockingClause(), sc)
	return out
}

// outputNames are the names ORDER BY may use for output columns.
func outputNames(out queryOutput) map[string]bool {
	names := make(map[string]bool, len(out.order))
	for _, name := range out.order {
		names[name] = true
	}
	return names
}

func (c *refCollector) analyzeValues(lists []*pg_query.Node, sc *scope) queryOutput {
	out := newOutput()
	for _, list := range lists {
		items := list.GetList().GetItems()
		c.walkList(items, sc, nil)
		for i := range items {
			out.add(fmt.Sprintf("column%d", i+1), nil)
		}
	}
	return out
}

// analyzeTargets records a target or RETURNING list and returns its
// output columns.
func (c *refCollector) analyzeTargets(targets []*pg_query.Node, sc *scope) queryOutput {
	out := newOutput()
	for _, node := range targets {
		res := node.GetResTarget()
		ref := res.GetVal().GetColumnRef()
		if ref == nil {
			c.walkExpr(res.GetVal(), sc, nil)
			out.add(targetName(res), nil)
			continue
		}
		pairs, starred, isStar := c.resolveRef(ref, sc, nil)
		if isStar {
			out.absorbStar(starred)
			continue
		}
		name := res.GetName()
		if names := stringList(ref.GetFields()); name == "" && len(names) > 0 {
			name = names[len(names)-1]
		}
		out.add(name, pairs)
	}
	return out
}

// targetName is the output name of a non-column target: its alias, or the
// function name PostgreSQL would give it.
func targetName(res *pg_query.ResTarget) string {
	if res.GetName() != "" {
		return res.GetName()
	}
	if call := res.GetVal().GetFuncCall(); call != nil {
		return nameFromList(call.GetFuncname()).Name
	}
	return ""
}

// applyLocking raises FOR UPDATE/SHARE targets of this level to ROW SHARE.
func (c *refCollector) applyLocking(clauses []*pg_query.Node, sc *scope) {
	for _, node := range clauses {
		locked := map[string]bool{}
		for _, rel := range node.GetLockingClause().GetLockedRels() {
			locked[rel.GetRangeVar().GetRelname()] = true
		}
		for _, e := range sc.entries {
			if e.kind == entryBase && (len(locked) == 0 || locked[e.alias]) {
				c.lockRelation(e.relation, LockRowShare)
			}
		}
	}
}

// addCTEs analyzes each CTE and makes it visible to later CTEs and to the
// statement (and, in WITH RECURSIVE, to itself).
func (c *refCollector) addCTEs(with *pg_query.WithClause, sc *scope) {
	for _, node := range with.GetCtes() {
		cte := node.GetCommonTableExpr()
		name := cte.GetCtename()
		c.ctes[name] = true
		if with.GetRecursive() {
			placeholder := newOutput()
			placeholder.open = true
			sc.ctes[name] = &placeholder
		}
		out := c.analyzeStatement(cte.GetCtequery(), sc)
		if names := stringList(cte.GetAliascolnames()); len(names) > 0 {
			out = out.renamed(names)
		}
		sc.ctes[name] = &out
	}
}

// analyzeStatement records a CTE body or subquery of any DML kind.
func (c *refCollector) analyzeStatement(node *pg_query.Node, sc *scope) queryOutput {
	switch {
	case node.GetSelectStmt() != nil:
		return c.analyzeSelect(node.GetSelectStmt(), sc)
	case node.GetInsertStmt() != nil:
		return c.analyzeInsert(node.GetInsertStmt(), sc)
	case node.GetUpdateStmt() != nil:
		return c.analyzeUpdate(node.GetUpdateStmt(), sc)
	case node.GetDeleteStmt() != nil:
		return c.analyzeDelete(node.GetDeleteStmt(), sc)
	case node.GetMergeStmt() != nil:
		return c.analyzeMerge(node.GetMergeStmt(), sc)
	}
	c.walkExpr(node, sc, nil)
	out := newOutput()
	out.open = true
	return out
}

func (c *refCollector) addFromItem(item *pg_query.Node, sc *scope) {
	switch {
	case item.GetRangeVar() != nil:
		c.addRangeVar(item.GetRangeVar(), sc)
	case item.GetRangeSubselect() != nil:
		sub := item.GetRangeSubselect()
		out := c.analyzeStatement(sub.GetSubquery(), sc)
		if names := stringList(sub.GetAlias().GetColnames()); len(names) > 0 {
			out = out.renamed(names)
		}
		sc.entries = append(sc.entries, &rangeEntry{kind: entryDerived,
			alias: sub.GetAlias().GetAliasname(), output: out})
	case item.GetRangeFunction() != nil:
		c.addRangeFunction(item.GetRangeFunction(), sc)
	case item.GetJoinExpr() != nil:
		c.addJoin(item.GetJoinExpr(), sc)
	case item.GetRangeTableSample() != nil:
		sample := item.GetRangeTableSample()
		c.addFromItem(sample.GetRelation(), sc)
		c.walkList(sample.GetArgs(), sc, nil)
		c.walkExpr(sample.GetRepeatable(), sc, nil)
	default:
		// XMLTABLE, JSON_TABLE and the like: record what they read and let
		// them own any column, so a bare column next to them is ambiguous.
		c.walkExpr(item, sc, nil)
		out := newOutput()
		out.open = true
		sc.entries = append(sc.entries, &rangeEntry{kind: entryOpaque, alias: aliasOf(item),
			output: out})
	}
}

// aliasOf reads the alias of any FROM item node that has one.
func aliasOf(item *pg_query.Node) string {
	msg := item.ProtoReflect()
	field := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("node"))
	if field == nil {
		return ""
	}
	inner := msg.Get(field).Message()
	aliasField := inner.Descriptor().Fields().ByName("alias")
	if aliasField == nil || !inner.Has(aliasField) {
		return ""
	}
	alias, ok := inner.Get(aliasField).Message().Interface().(*pg_query.Alias)
	if !ok {
		return ""
	}
	return alias.GetAliasname()
}

// addRangeVar adds a FROM relation, or a CTE when an unqualified name
// matches a visible CTE.
func (c *refCollector) addRangeVar(rv *pg_query.RangeVar, sc *scope) {
	name := rangeVarName(rv)
	alias := rv.GetAlias().GetAliasname()
	visible := alias
	if visible == "" {
		visible = name.Name
	}
	if cte, ok := sc.findCTE(name.Name); ok && name.Schema == "" {
		out := *cte
		if names := stringList(rv.GetAlias().GetColnames()); len(names) > 0 {
			out = out.renamed(names)
		}
		sc.entries = append(sc.entries, &rangeEntry{kind: entryDerived, alias: visible,
			output: out})
		return
	}
	c.lockRelation(name, LockAccessShare)
	sc.entries = append(sc.entries, &rangeEntry{kind: entryBase, alias: visible,
		hasAlias: alias != "", relation: name})
}

func (c *refCollector) addRangeFunction(rf *pg_query.RangeFunction, sc *scope) {
	var names []string
	visible := rf.GetAlias().GetAliasname()
	for _, fn := range rf.GetFunctions() {
		items := fn.GetList().GetItems()
		if len(items) > 0 {
			c.walkExpr(items[0], sc, nil)
			if call := items[0].GetFuncCall(); call != nil && visible == "" {
				visible = nameFromList(call.GetFuncname()).Name
			}
		}
		if len(items) > 1 {
			names = append(names, columnDefNames(items[1].GetList().GetItems())...)
		}
	}
	names = append(names, columnDefNames(rf.GetColdeflist())...)
	if aliased := stringList(rf.GetAlias().GetColnames()); len(aliased) > 0 {
		names = aliased
	}
	if len(names) == 0 {
		names = []string{visible}
	}
	out := newOutput()
	for _, name := range names {
		out.add(name, nil)
	}
	if rf.GetOrdinality() {
		out.add("ordinality", nil)
	}
	sc.entries = append(sc.entries, &rangeEntry{kind: entryOpaque, alias: visible,
		output: out})
}

func columnDefNames(defs []*pg_query.Node) []string {
	var names []string
	for _, def := range defs {
		if name := def.GetColumnDef().GetColname(); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// addJoin adds both sides; each USING column is owned by one entry per
// side and merged, so a later bare reference is not ambiguous.
func (c *refCollector) addJoin(join *pg_query.JoinExpr, sc *scope) {
	before := len(sc.entries)
	c.addFromItem(join.GetLarg(), sc)
	mid := len(sc.entries)
	c.addFromItem(join.GetRarg(), sc)
	left := append([]*rangeEntry(nil), sc.entries[before:mid]...)
	right := append([]*rangeEntry(nil), sc.entries[mid:]...)
	for _, column := range stringList(join.GetUsingClause()) {
		sides := sc.using[column]
		if len(sides) == 0 {
			sides = soleOwner(left, column)
		}
		sides = append(sides, soleOwner(right, column)...)
		for _, side := range sides {
			c.addColumns(side.column(column))
		}
		sc.using[column] = sides
	}
	c.walkExpr(join.GetQuals(), sc, nil)
}

func soleOwner(entries []*rangeEntry, column string) []*rangeEntry {
	var owners []*rangeEntry
	for _, e := range entries {
		if e.hasColumn(column) {
			owners = append(owners, e)
		}
	}
	if len(owners) != 1 {
		return nil
	}
	return owners
}
