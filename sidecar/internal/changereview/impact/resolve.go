package impact

import (
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// resolvePart fills the schema of every relation name in p, registers the
// relations whose context the rules need and records p's target relation
// (for DROP INDEX, the table of the index).
func (r *run) resolvePart(s *stmt, p *sqlast.DDLStatement) {
	for _, n := range partNames(p) {
		r.resolveName(s, n)
	}
	target := qualified(p.Relation)
	switch p.Kind {
	case sqlast.DDLDropIndex:
		if p.Object.Schema == "" {
			r.resolveName(s, &p.Object)
		}
		target = r.indexTable(s, p.Object)
	case sqlast.DDLCreateTable:
		r.created[target] = true
	}
	s.targets = append(s.targets, target)
	if target != "" {
		r.touch(s, target)
	}
	for _, ref := range partRefs(p) {
		r.relationFor(ref)
	}
}

// touch registers rel as a relation s changes.
func (r *run) touch(s *stmt, rel string) {
	schema, name, ok := splitQualified(rel)
	if !ok {
		return
	}
	r.relationFor(sqlast.QualifiedName{Schema: schema, Name: name})
	if !hasString(s.relations, rel) {
		s.relations = append(s.relations, rel)
	}
}

// partNames is every relation name p carries, to resolve in place.
func partNames(p *sqlast.DDLStatement) []*sqlast.QualifiedName {
	names := []*sqlast.QualifiedName{&p.Relation}
	addConstraints := func(cs []sqlast.ConstraintDef) {
		for i := range cs {
			if cs[i].RefRelation != nil {
				names = append(names, cs[i].RefRelation)
			}
		}
	}
	if p.Constraint != nil && p.Constraint.RefRelation != nil {
		names = append(names, p.Constraint.RefRelation)
	}
	if p.Column != nil {
		addConstraints(p.Column.Constraints)
	}
	if t := p.Table; t != nil {
		addConstraints(t.Constraints)
		for i := range t.Columns {
			addConstraints(t.Columns[i].Constraints)
		}
		if t.PartitionOf != nil {
			names = append(names, t.PartitionOf)
		}
		for i := range t.Inherits {
			names = append(names, &t.Inherits[i])
		}
	}
	return names
}

// partRefs is the relations p references without changing them.
func partRefs(p *sqlast.DDLStatement) []sqlast.QualifiedName {
	var out []sqlast.QualifiedName
	for _, fk := range foreignKeys(p) {
		out = append(out, *fk.def.RefRelation)
	}
	if p.Table != nil && p.Table.PartitionOf != nil {
		out = append(out, *p.Table.PartitionOf)
	}
	return out
}

// newFK is one foreign key a part adds: a table or column constraint.
type newFK struct {
	def     sqlast.ConstraintDef
	columns []string
}

// foreignKeys is every foreign key p adds, with its referencing columns.
func foreignKeys(p *sqlast.DDLStatement) []newFK {
	var out []newFK
	add := func(c sqlast.ConstraintDef, col string) {
		if c.Kind != sqlast.ConstraintForeignKey || c.RefRelation == nil {
			return
		}
		cols := c.Columns
		if len(cols) == 0 && col != "" {
			cols = []string{col}
		}
		out = append(out, newFK{def: c, columns: cols})
	}
	if p.Kind == sqlast.DDLAddConstraint && p.Constraint != nil {
		add(*p.Constraint, "")
	}
	if p.Column != nil {
		for _, c := range p.Column.Constraints {
			add(c, p.Column.Name)
		}
	}
	if p.Table != nil {
		for _, c := range p.Table.Constraints {
			add(c, "")
		}
		for _, col := range p.Table.Columns {
			for _, c := range col.Constraints {
				add(c, col.Name)
			}
		}
	}
	return out
}
