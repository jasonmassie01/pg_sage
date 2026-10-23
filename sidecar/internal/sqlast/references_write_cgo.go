//go:build cgo

package sqlast

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// writeTarget locks a written relation ROW EXCLUSIVE and makes it visible
// at sc under its alias.
func (c *refCollector) writeTarget(rv *pg_query.RangeVar, sc *scope) QualifiedName {
	c.modifies = true
	name := rangeVarName(rv)
	c.lockRelation(name, LockRowExclusive)
	alias := rv.GetAlias().GetAliasname()
	visible := alias
	if visible == "" {
		visible = name.Name
	}
	sc.entries = append(sc.entries, &rangeEntry{kind: entryBase, alias: visible,
		hasAlias: alias != "", relation: name})
	return name
}

// setTargets records SET (or INSERT) target columns of the written
// relation and the expressions assigned to them.
func (c *refCollector) setTargets(targets []*pg_query.Node, target QualifiedName, sc *scope) {
	for _, node := range targets {
		res := node.GetResTarget()
		if res.GetName() != "" {
			c.addColumns([]ColumnPair{{Relation: target, Column: res.GetName()}})
		}
		c.walkExpr(res.GetVal(), sc, nil)
	}
}

func (c *refCollector) analyzeInsert(ins *pg_query.InsertStmt, parent *scope) queryOutput {
	sc := newScope(parent)
	c.addCTEs(ins.GetWithClause(), sc)
	if sel := ins.GetSelectStmt().GetSelectStmt(); sel != nil {
		c.analyzeSelect(sel, sc)
	}
	target := c.writeTarget(ins.GetRelation(), sc)
	c.setTargets(ins.GetCols(), target, sc)
	if conflict := ins.GetOnConflictClause(); conflict != nil {
		c.analyzeOnConflict(conflict, target, sc)
	}
	return c.analyzeTargets(ins.GetReturningList(), sc)
}

// analyzeOnConflict records the conflict target and DO UPDATE clause, where
// excluded names the proposed row (only qualified references reach it).
func (c *refCollector) analyzeOnConflict(oc *pg_query.OnConflictClause, target QualifiedName,
	sc *scope) {
	infer := oc.GetInfer()
	for _, node := range infer.GetIndexElems() {
		elem := node.GetIndexElem()
		if elem.GetName() != "" {
			c.addColumns([]ColumnPair{{Relation: target, Column: elem.GetName()}})
		}
		c.walkExpr(elem.GetExpr(), sc, nil)
	}
	c.walkExpr(infer.GetWhereClause(), sc, nil)
	sc.entries = append(sc.entries, &rangeEntry{kind: entryBase, alias: "excluded",
		hasAlias: true, qualifiedOnly: true, relation: target})
	c.setTargets(oc.GetTargetList(), target, sc)
	c.walkExpr(oc.GetWhereClause(), sc, nil)
}

func (c *refCollector) analyzeUpdate(upd *pg_query.UpdateStmt, parent *scope) queryOutput {
	sc := newScope(parent)
	c.addCTEs(upd.GetWithClause(), sc)
	target := c.writeTarget(upd.GetRelation(), sc)
	for _, item := range upd.GetFromClause() {
		c.addFromItem(item, sc)
	}
	c.setTargets(upd.GetTargetList(), target, sc)
	c.walkExpr(upd.GetWhereClause(), sc, nil)
	return c.analyzeTargets(upd.GetReturningList(), sc)
}

func (c *refCollector) analyzeDelete(del *pg_query.DeleteStmt, parent *scope) queryOutput {
	sc := newScope(parent)
	c.addCTEs(del.GetWithClause(), sc)
	c.writeTarget(del.GetRelation(), sc)
	for _, item := range del.GetUsingClause() {
		c.addFromItem(item, sc)
	}
	c.walkExpr(del.GetWhereClause(), sc, nil)
	return c.analyzeTargets(del.GetReturningList(), sc)
}

func (c *refCollector) analyzeMerge(merge *pg_query.MergeStmt, parent *scope) queryOutput {
	sc := newScope(parent)
	c.addCTEs(merge.GetWithClause(), sc)
	target := c.writeTarget(merge.GetRelation(), sc)
	c.addFromItem(merge.GetSourceRelation(), sc)
	c.walkExpr(merge.GetJoinCondition(), sc, nil)
	for _, node := range merge.GetMergeWhenClauses() {
		when := node.GetMergeWhenClause()
		c.walkExpr(when.GetCondition(), sc, nil)
		c.setTargets(when.GetTargetList(), target, sc)
		c.walkList(when.GetValues(), sc, nil)
	}
	return c.analyzeTargets(merge.GetReturningList(), sc)
}
