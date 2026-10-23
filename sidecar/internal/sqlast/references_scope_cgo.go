//go:build cgo

package sqlast

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type entryKind int

const (
	// entryBase is a table, view or other catalog relation.
	entryBase entryKind = iota
	// entryDerived is a subquery or CTE with known output columns.
	entryDerived
	// entryOpaque is a function or table function in FROM.
	entryOpaque
)

// rangeEntry is one FROM item visible at a query level.
type rangeEntry struct {
	kind     entryKind
	alias    string // the name the query refers to it by
	hasAlias bool   // a base relation renamed by an explicit alias
	// qualifiedOnly entries (ON CONFLICT's excluded) never own an
	// unqualified column.
	qualifiedOnly bool
	relation      QualifiedName
	output        queryOutput
}

func (e *rangeEntry) displayName() QualifiedName {
	if e.kind == entryBase {
		return e.relation
	}
	return QualifiedName{Name: e.alias}
}

// hasColumn reports whether the entry could own column: a base relation
// always could (there is no catalog), a derived entry when it lists the
// column or exposes a star.
func (e *rangeEntry) hasColumn(column string) bool {
	if e.qualifiedOnly {
		return false
	}
	return e.kind == entryBase || e.output.has(column)
}

// column returns the base (relation, column) pairs the entry's column
// stands for.
func (e *rangeEntry) column(column string) []ColumnPair {
	if e.kind == entryBase {
		return []ColumnPair{{Relation: e.relation, Column: column}}
	}
	return e.output.lineage(column)
}

// queryOutput is what a subquery or CTE yields: its named columns with the
// base columns each stands for.
type queryOutput struct {
	order   []string
	columns map[string][]ColumnPair
	// open is set when the output has columns that are not listed (a star
	// over a relation or function).
	open bool
	// starBase is the single base relation a star exposes, so any column
	// name maps to it.
	starBase *QualifiedName
}

func newOutput() queryOutput { return queryOutput{columns: map[string][]ColumnPair{}} }

func (o *queryOutput) add(name string, pairs []ColumnPair) {
	if name == "" {
		return
	}
	if _, ok := o.columns[name]; !ok {
		o.order = append(o.order, name)
	}
	o.columns[name] = append(o.columns[name], pairs...)
}

func (o queryOutput) has(column string) bool {
	_, ok := o.columns[column]
	return ok || o.open
}

func (o queryOutput) lineage(column string) []ColumnPair {
	if pairs, ok := o.columns[column]; ok {
		return pairs
	}
	if o.starBase != nil {
		return []ColumnPair{{Relation: *o.starBase, Column: column}}
	}
	return nil
}

// renamed applies a column alias list positionally.
func (o queryOutput) renamed(names []string) queryOutput {
	out := newOutput()
	out.open, out.starBase = o.open, o.starBase
	for i, name := range o.order {
		if i < len(names) {
			out.add(names[i], o.columns[name])
		} else {
			out.add(name, o.columns[name])
		}
	}
	for i := len(o.order); i < len(names); i++ {
		out.add(names[i], nil)
	}
	return out
}

// absorbStar adds what a star over entries yields.
func (o *queryOutput) absorbStar(entries []*rangeEntry) {
	for _, e := range entries {
		switch e.kind {
		case entryBase:
			o.open = true
			if len(entries) == 1 {
				rel := e.relation
				o.starBase = &rel
			}
		default:
			for _, name := range e.output.order {
				o.add(name, e.output.columns[name])
			}
			o.open = o.open || e.output.open
			if len(entries) == 1 {
				o.starBase = e.output.starBase
			}
		}
	}
}

// scope is one query level: its FROM entries, JOIN USING columns and CTEs.
type scope struct {
	parent  *scope
	entries []*rangeEntry
	using   map[string][]*rangeEntry
	ctes    map[string]*queryOutput
}

func newScope(parent *scope) *scope {
	return &scope{parent: parent, using: map[string][]*rangeEntry{},
		ctes: map[string]*queryOutput{}}
}

func (s *scope) findCTE(name string) (*queryOutput, bool) {
	for level := s; level != nil; level = level.parent {
		if out, ok := level.ctes[name]; ok {
			return out, true
		}
	}
	return nil, false
}

func (s *scope) findAlias(alias string) *rangeEntry {
	for level := s; level != nil; level = level.parent {
		for _, e := range level.entries {
			if e.alias == alias {
				return e
			}
		}
	}
	return nil
}

// findQualified finds an unaliased base relation by schema and name.
func (s *scope) findQualified(schema, name string) *rangeEntry {
	for level := s; level != nil; level = level.parent {
		for _, e := range level.entries {
			if e.kind == entryBase && !e.hasAlias && e.relation.Schema == schema &&
				e.relation.Name == name {
				return e
			}
		}
	}
	return nil
}

// resolveRef records a column reference and returns the base columns it
// stands for, or, for a star, the entries it expands.
func (c *refCollector) resolveRef(ref *pg_query.ColumnRef, sc *scope,
	skip map[string]bool) ([]ColumnPair, []*rangeEntry, bool) {
	fields := ref.GetFields()
	names := stringList(fields)
	if len(fields) > 0 && fields[len(fields)-1].GetAStar() != nil {
		return nil, c.starEntries(names, sc), true
	}
	switch len(names) {
	case 0:
		return nil, nil, false
	case 1:
		if skip[names[0]] {
			return nil, nil, false
		}
		return c.resolveUnqualified(names[0], sc), nil, false
	}
	entry, column := lookupQualified(names, sc)
	if entry == nil {
		c.unresolved[strings.Join(names, ".")] = true
		return nil, nil, false
	}
	pairs := entry.column(column)
	c.addColumns(pairs)
	return pairs, nil, false
}

// lookupQualified resolves schema.table.column first, then alias.column
// (a longer name is a field of a composite column).
func lookupQualified(names []string, sc *scope) (*rangeEntry, string) {
	n := len(names)
	if n >= 3 {
		if e := sc.findQualified(names[n-3], names[n-2]); e != nil {
			return e, names[n-1]
		}
	}
	if e := sc.findAlias(names[0]); e != nil {
		return e, names[1]
	}
	return nil, ""
}

func (c *refCollector) starEntries(qualifier []string, sc *scope) []*rangeEntry {
	var entries []*rangeEntry
	switch len(qualifier) {
	case 0:
		if sc != nil {
			entries = sc.entries
		}
	case 1:
		if e := sc.findAlias(qualifier[0]); e != nil {
			entries = []*rangeEntry{e}
		}
	default:
		n := len(qualifier)
		if e := sc.findQualified(qualifier[n-2], qualifier[n-1]); e != nil {
			entries = []*rangeEntry{e}
		}
	}
	if len(qualifier) > 0 && len(entries) == 0 {
		c.unresolved[strings.Join(qualifier, ".")+".*"] = true
	}
	for _, e := range entries {
		c.markStar(e)
	}
	return entries
}

func (c *refCollector) markStar(e *rangeEntry) {
	switch e.kind {
	case entryBase:
		c.star[e.relation] = true
	case entryDerived:
		for _, name := range e.output.order {
			c.addColumns(e.output.columns[name])
		}
	}
}

// resolveUnqualified attributes a bare column to the innermost level with
// a candidate owner: exactly one owner resolves it, several make it
// ambiguous, none anywhere leaves it unresolved.
func (c *refCollector) resolveUnqualified(column string, sc *scope) []ColumnPair {
	for level := sc; level != nil; level = level.parent {
		if sides, ok := level.using[column]; ok {
			var pairs []ColumnPair
			for _, side := range sides {
				pairs = append(pairs, side.column(column)...)
			}
			c.addColumns(pairs)
			return pairs
		}
		var candidates []*rangeEntry
		for _, e := range level.entries {
			if e.hasColumn(column) {
				candidates = append(candidates, e)
			}
		}
		switch len(candidates) {
		case 0:
			continue
		case 1:
			pairs := candidates[0].column(column)
			c.addColumns(pairs)
			return pairs
		default:
			c.addAmbiguous(column, candidates)
			return nil
		}
	}
	c.unresolved[column] = true
	return nil
}

// walkExpr records every column reference and subquery inside node.
func (c *refCollector) walkExpr(node *pg_query.Node, sc *scope, skip map[string]bool) {
	if node == nil {
		return
	}
	visitTree(node.ProtoReflect(), func(msg protoreflect.Message) bool {
		switch n := msg.Interface().(type) {
		case *pg_query.ColumnRef:
			c.resolveRef(n, sc, skip)
			return false
		case *pg_query.SelectStmt:
			c.analyzeSelect(n, sc)
			return false
		case *pg_query.RangeVar:
			c.lockRelation(rangeVarName(n), LockAccessShare)
			return false
		}
		return true
	})
}

func (c *refCollector) walkList(nodes []*pg_query.Node, sc *scope, skip map[string]bool) {
	for _, node := range nodes {
		c.walkExpr(node, sc, skip)
	}
}
