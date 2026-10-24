package impact

import (
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// runRules is stage 4 for one statement: CR-01 over the statement, the
// part rules, then the plan rules (stage 5).
func (r *run) runRules(s *stmt) {
	if s.text {
		r.lockRules(s, nil)
		return
	}
	expand := r.expandSteps(s)
	as := r.lockRules(s, expand)
	parents := map[string]bool{}
	for i := range s.parts {
		r.partRules(s, i, as, expand[i], parents)
		if r.err != nil {
			return
		}
	}
	r.unmodeled(s, len(as) > 0)
	r.planRules(s)
}

// partRules runs the rules of part i's kind against its relation.
func (r *run) partRules(s *stmt, i int, as []assessment, expandID string,
	parents map[string]bool) {
	p := s.parts[i]
	if p.Kind == sqlast.DDLCreateIndex || p.Kind == sqlast.DDLDropIndex {
		r.concurrentInTransaction(s, i)
	}
	rel := r.target(s, i)
	if rel == nil {
		r.missingIndex(s, i)
		r.otherObject(s, i)
		return
	}
	if !rel.found {
		r.newRelationRules(s, i, rel)
		return
	}
	if _, failed := rel.failed["relation"]; failed {
		return
	}
	r.lockQueue(s, i, as)
	if structural[p.Kind] && !parents[rel.name] {
		parents[rel.name] = true
		r.partitioned(s, rel)
	}
	r.kindRules(s, i, rel, as, expandID)
}

// newRelationRules is a part on a relation the catalog does not hold: a
// table the script creates, or unknown.
func (r *run) newRelationRules(s *stmt, i int, rel *relation) {
	p := s.parts[i]
	switch {
	case p.Kind == sqlast.DDLCreateTable:
		r.newTable(s, i)
		r.foreignKeyRules(s, i, rel)
	case rel.created:
		r.foreignKeyRules(s, i, rel)
	case !p.IfExists:
		r.unknownFact(s, rel.name, "not found in the catalog", "the change targets a "+
			"relation this database does not have", "")
	}
}

func (r *run) kindRules(s *stmt, i int, rel *relation, as []assessment, expandID string) {
	p := s.parts[i]
	switch p.Kind {
	case sqlast.DDLRenameColumn:
		r.columnRemoved(s, i, rel, p.Columns[0], "renamed to "+p.NewName)
	case sqlast.DDLDropColumn:
		r.columnRemoved(s, i, rel, p.Column.Name, "dropped")
	case sqlast.DDLAlterType:
		r.alterType(s, i, rel, as)
		r.publishedFinding(s, rel, changereview.SeverityWarning,
			"subscribers must accept the new type of "+p.Column.Name+".")
	case sqlast.DDLAddColumn:
		r.addColumn(s, i, rel)
		r.foreignKeyRules(s, i, rel)
	case sqlast.DDLSetNotNull:
		r.setNotNull(s, i, rel, expandID)
	case sqlast.DDLCreateIndex:
		r.createIndex(s, i, rel)
	case sqlast.DDLDropIndex:
		r.dropIndex(s, i, rel)
	case sqlast.DDLAddConstraint:
		r.foreignKeyRules(s, i, rel)
	case sqlast.DDLDropTable:
		r.relationRemoved(s, i, rel, "dropped")
	case sqlast.DDLRenameTable:
		r.relationRemoved(s, i, rel, "renamed to "+p.NewName)
	case sqlast.DDLOther:
		r.otherRules(s, i, rel)
	}
}

// createIndex is CR-05 for CONCURRENTLY on a partitioned table, CR-08 and
// CR-09.
func (r *run) createIndex(s *stmt, i int, rel *relation) {
	if s.parts[i].Index.Concurrent && rel.relkind == "p" {
		r.find(RuleWillFail, changereview.SeverityCritical, "CREATE INDEX CONCURRENTLY "+
			"will fail on a partitioned table", "PostgreSQL cannot build an index "+
			"CONCURRENTLY on partitioned table "+rel.name+": create it ON ONLY the parent, "+
			"CONCURRENTLY on each partition, then ATTACH.", s.evID, rel.evID)
	}
	r.redundantIndexes(s, i, rel)
	r.whatIf(s, i, rel)
}

// expandSteps is migration/plan's expand/contract sequence for a lone ADD
// UNIQUE or SET NOT NULL, as evidence ids by part ("" for none).
func (r *run) expandSteps(s *stmt) []string {
	ids := make([]string, len(s.parts))
	if len(s.parts) != 1 {
		return ids
	}
	p := s.parts[0]
	unique := p.Kind == sqlast.DDLAddConstraint && p.Constraint != nil &&
		p.Constraint.Kind == sqlast.ConstraintUnique && p.Constraint.Name != ""
	rel := r.target(s, 0)
	if (!unique && p.Kind != sqlast.DDLSetNotNull) || rel == nil || !rel.found {
		return ids
	}
	pl, err := plan.NewPlanner().Plan(r.ctx, plan.Request{SQL: p.Text, Cycle: 1,
		Table: plan.TableFacts{Schema: rel.schema, Name: rel.rel,
			EstimatedRows: int64(max(rel.reltuples, 0))}})
	if err != nil {
		r.e.logf("change review impact: no expand/contract plan for statement %d: %v",
			s.index, err)
		return ids
	}
	d := expandData{Classification: string(pl.Classification), Steps: []stepData{}}
	for _, st := range append(pl.ExpandSteps, pl.ContractSteps...) {
		d.Steps = append(d.Steps, stepData{Phase: string(st.Phase), SQL: st.SQL})
	}
	ids[0] = r.addEvidence("expand", partRef(s.index, 0), "expand/contract for "+
		string(pl.Classification), d)
	return ids
}

// missingIndex is a DROP INDEX of an index the catalog does not hold:
// the change fails unless the script creates it first or says IF EXISTS.
func (r *run) missingIndex(s *stmt, i int) {
	p := s.parts[i]
	if p.Kind != sqlast.DDLDropIndex || p.IfExists || r.createdIndex(s, p.Object.Name) {
		return
	}
	r.unknownFact(s, "index "+qualified(p.Object), "not found", "the catalog has no such "+
		"index, so the DROP fails", "")
}

// createdIndex reports whether a statement before s creates index name.
func (r *run) createdIndex(s *stmt, name string) bool {
	for _, o := range r.stmts {
		if o.index >= s.index {
			return false
		}
		for _, p := range o.parts {
			if p.Kind == sqlast.DDLCreateIndex && indexName(p) == name {
				return true
			}
		}
	}
	return false
}
