package impact

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// relationRemoved is DROP or RENAME of a table or view: CR-04 for the
// workload statements and function bodies still naming it, CR-05 or
// CR-04 for dependent views of a drop, CR-13 for a published table.
func (r *run) relationRemoved(s *stmt, i int, rel *relation, verb string) {
	p := s.parts[i]
	ref := referenced{known: r.workloadKnown(s, rel.name)}
	if ref.known {
		ref.stmts = r.work.byRelation[rel.name]
	}
	r.referenceFindings(s, RuleRelationReferenced, rel, rel.name, verb, ref)
	r.functionFindings(s, RuleRelationReferenced, rel.name, verb,
		r.functionsNaming(s, rel.rel))
	if p.Kind == sqlast.DDLDropTable {
		r.dependentViews(s, rel, "", p.Behavior == "cascade")
	}
	r.publishedFinding(s, rel, changereview.SeverityWarning,
		"subscribers still replicate "+rel.name+".")
}

// partitioned is CR-16: DDL on a partitioned or inheritance parent
// reaches every partition or child.
func (r *run) partitioned(s *stmt, rel *relation) {
	if rel.inherits == 0 && rel.relkind != "p" {
		return
	}
	what := fmt.Sprintf("%d inheritance children", rel.inherits)
	if rel.relkind == "p" {
		what = fmt.Sprintf("%d partitions", rel.inherits)
	}
	r.find(RulePartitioned, changereview.SeverityWarning, rel.name+" is a parent of "+what,
		fmt.Sprintf("%s has %s: the change locks and, where it rewrites or scans, works "+
			"through each of them; size and lock time are per child, not this table.",
			rel.name, what), s.evID, rel.evID)
}

// structural kinds reach partitions and children.
var structural = map[sqlast.DDLKind]bool{sqlast.DDLAddColumn: true,
	sqlast.DDLDropColumn: true, sqlast.DDLRenameColumn: true, sqlast.DDLAlterType: true,
	sqlast.DDLSetNotNull: true, sqlast.DDLDropNotNull: true, sqlast.DDLSetDefault: true,
	sqlast.DDLAddConstraint: true, sqlast.DDLDropConstraint: true,
	sqlast.DDLValidateConstraint: true, sqlast.DDLCreateIndex: true,
	sqlast.DDLDropTable: true}

// newTable is CR-15 for CREATE TABLE: no primary key (warning), an int4
// primary key and timestamp without time zone (info).
func (r *run) newTable(s *stmt, i int) {
	p := s.parts[i]
	t := p.Table
	if t == nil || t.PartitionOf != nil || t.Temporary {
		return
	}
	name := qualified(p.Relation)
	pk := primaryKey(t)
	types := map[string]string{}
	var stamps []string
	for _, c := range t.Columns {
		types[c.Name] = formatType(c.Type)
		if types[c.Name] == "timestamp without time zone" {
			stamps = append(stamps, c.Name)
		}
	}
	switch {
	case len(pk) == 0:
		r.find(RuleNewObject, changereview.SeverityWarning, name+" has no primary key",
			"A table without a primary key cannot be replicated logically with UPDATE or "+
				"DELETE, and rows cannot be addressed reliably. Add one.", s.evID)
	case len(pk) == 1 && smallInt(types[pk[0]]):
		r.find(RuleNewObject, changereview.SeverityInfo, name+" uses an int4 primary key",
			"Primary key "+pk[0]+" is "+types[pk[0]]+": it runs out at 2.1 billion rows; "+
				"bigint costs 4 bytes more per row.", s.evID)
	}
	if len(stamps) > 0 {
		r.find(RuleNewObject, changereview.SeverityInfo, name+
			" uses timestamp without time zone", "Columns "+strings.Join(stamps, ", ")+
			" store timestamp without time zone; timestamptz keeps the instant whatever "+
			"the session time zone.", s.evID)
	}
}

// primaryKey is a new table's primary key columns.
func primaryKey(t *sqlast.TableDef) []string {
	for _, c := range t.Constraints {
		if c.Kind == sqlast.ConstraintPrimaryKey {
			return c.Columns
		}
	}
	for _, col := range t.Columns {
		for _, c := range col.Constraints {
			if c.Kind == sqlast.ConstraintPrimaryKey {
				return []string{col.Name}
			}
		}
	}
	return nil
}

func smallInt(t string) bool {
	switch t {
	case "integer", "smallint", "serial", "smallserial":
		return true
	}
	return false
}
