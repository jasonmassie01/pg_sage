package impact

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// columnRemoved is DROP COLUMN or RENAME COLUMN of part i: CR-03 for the
// statements and functions still using it, CR-05 or CR-03 for dependent
// views, CR-13 for a published table and CR-17 when usage is unknown.
func (r *run) columnRemoved(s *stmt, i int, rel *relation, col, verb string) {
	p := s.parts[i]
	name := rel.name + "." + col
	r.referenceFindings(s, RuleColumnReferenced, rel, name, verb, r.columnUsers(s, rel, col))
	r.functionFindings(s, RuleColumnReferenced, name, verb, r.functionsNaming(s, col))
	if p.Kind == sqlast.DDLDropColumn {
		r.dependentViews(s, rel, col, p.Behavior == "cascade")
	}
	r.publishedFinding(s, rel, changereview.SeverityWarning,
		"subscribers still expect "+name+".")
}

// viewsOn is the views of rel that read col ("" for any column).
func viewsOn(rel *relation, col string) []viewDep {
	var out []viewDep
	for _, v := range rel.views {
		if col == "" || hasString(v.columns, col) {
			out = append(out, v)
		}
	}
	return out
}

// dependentViews is a drop under views: without CASCADE PostgreSQL
// refuses it (CR-05); with CASCADE it drops the views too (CR-03/CR-04).
// Views the script drops first do not count; views it redefines first
// may no longer depend on the column (a CR-05 warning).
func (r *run) dependentViews(s *stmt, rel *relation, col string, cascade bool) {
	if _, failed := rel.failed[readViews]; failed {
		return
	}
	views, redefined := r.viewsAt(s, viewsOn(rel, col))
	what := rel.name
	if col != "" {
		what += "." + col
	}
	if len(views) == 0 {
		r.redefinedViews(s, rel, what, "dropping", redefined)
		return
	}
	ids, names := r.viewEvidence(s, rel, views)
	if cascade {
		rule := RuleColumnReferenced
		if col == "" {
			rule = RuleRelationReferenced
		}
		r.find(rule, changereview.SeverityCritical, "Dropping "+what+" also drops views "+
			names, "CASCADE drops the dependent views "+names+" and everything reading "+
			"them.", ids...)
		return
	}
	r.find(RuleWillFail, changereview.SeverityCritical, "Dropping "+what+" will fail: "+
		"views depend on it", "PostgreSQL refuses to drop "+what+" because views "+names+
		" depend on it; the migration fails.", ids...)
}

// viewsAt is the catalog views of rel as they stand when s runs: those
// the script drops earlier are gone; those it redefines earlier are
// returned apart (whether they still read the column is not known).
func (r *run) viewsAt(s *stmt, views []viewDep) ([]viewDep, []viewDep) {
	var kept, redefined []viewDep
	for _, v := range views {
		switch r.earlierViewChange(s, v.name) {
		case "dropped":
		case "redefined":
			redefined = append(redefined, v)
		default:
			kept = append(kept, v)
		}
	}
	return kept, redefined
}

// earlierViewChange is what a statement before s does to view: "dropped",
// "redefined" (CREATE OR REPLACE VIEW) or "".
func (r *run) earlierViewChange(s *stmt, view string) string {
	change := ""
	for _, o := range r.stmts {
		if o.index >= s.index {
			break
		}
		for i, p := range o.parts {
			if i >= len(o.targets) || o.targets[i] != view {
				continue
			}
			switch {
			case p.Kind == sqlast.DDLDropTable:
				change = "dropped"
			case p.Kind == sqlast.DDLOther && p.Command == "ViewStmt":
				change = "redefined"
			}
		}
	}
	return change
}

// redefinedViews is the CR-05 warning for a change under views the script
// redefines first.
func (r *run) redefinedViews(s *stmt, rel *relation, what, verb string,
	views []viewDep) {
	if len(views) == 0 {
		return
	}
	ids, names := r.viewEvidence(s, rel, views)
	r.find(RuleWillFail, changereview.SeverityWarning, verb+" "+what+" may fail: views "+
		"are redefined first", "The script redefines views "+names+" before "+verb+" "+
		what+"; if the new definitions still use it, PostgreSQL refuses the change and "+
		"the migration fails.", ids...)
}

func (r *run) viewEvidence(s *stmt, rel *relation, views []viewDep) ([]string, string) {
	ids := []string{s.evID, rel.evID}
	var names []string
	for _, v := range views {
		ids = append(ids, r.addEvidence("view", v.name, v.name+" depends on "+rel.name,
			viewData{View: v.name, Relation: rel.name, Columns: v.columns}))
		names = append(names, v.name)
	}
	return ids, strings.Join(names, ", ")
}

// alterType is ALTER COLUMN ... TYPE: CR-05 under a view, CR-06 for the
// rewrite or its absence.
func (r *run) alterType(s *stmt, i int, rel *relation, as []assessment) {
	p := s.parts[i]
	col, target := p.Column.Name, formatType(p.Column.Type)
	views, redefined := r.viewsAt(s, viewsOn(rel, col))
	if len(views) > 0 {
		ids, names := r.viewEvidence(s, rel, views)
		r.find(RuleWillFail, changereview.SeverityCritical, "Changing the type of "+
			rel.name+"."+col+" will fail: views use it", "PostgreSQL cannot alter the type "+
			"of a column used by a view or rule ("+names+"); the migration fails.", ids...)
	} else {
		r.redefinedViews(s, rel, rel.name+"."+col, "changing the type of", redefined)
	}
	cur, ok := rel.columns[col]
	if !ok {
		r.unknownFact(s, "type of "+rel.name+"."+col, "column not found", "", "")
		return
	}
	ids := []string{s.evID, rel.evID}
	for _, a := range as {
		ids = append(ids, a.evID)
	}
	coercible := false
	for _, a := range as {
		coercible = coercible || a.coercion
	}
	if coercible {
		r.find(RuleTypeChange, changereview.SeverityInfo, "binary-coercible: no rewrite",
			fmt.Sprintf("%s.%s goes from %s to %s, which is binary-coercible: no rewrite "+
				"and no index rebuild, but ACCESS EXCLUSIVE is taken briefly.", rel.name,
				col, cur.typ, target), ids...)
		return
	}
	r.find(RuleTypeChange, changereview.SeverityWarning, "Type change rewrites "+rel.name,
		fmt.Sprintf("%s.%s goes from %s to %s: PostgreSQL rewrites %s (%d bytes, %.0f "+
			"rows) and rebuilds its %d indexes under ACCESS EXCLUSIVE.", rel.name, col,
			cur.typ, target, rel.name, rel.totalBytes, rel.reltuples, len(rel.indexes)),
		ids...)
}

// addColumn is ADD COLUMN: CR-05 for NOT NULL without a default on a
// table with rows, CR-13 info, CR-15 for timestamp without time zone.
func (r *run) addColumn(s *stmt, i int, rel *relation) {
	c := s.parts[i].Column
	if c.NotNull && !c.HasDefault && c.Generated == "" && !rel.created {
		switch {
		case rel.reltuples > 0:
			r.find(RuleWillFail, changereview.SeverityCritical, "ADD COLUMN "+c.Name+
				" NOT NULL without a default will fail", fmt.Sprintf("%s holds about %.0f "+
				"rows; a NOT NULL column without a DEFAULT cannot be added to a table "+
				"with rows. Add it with a DEFAULT, or nullable and backfill.", rel.name,
				rel.reltuples), s.evID, rel.evID)
		case rel.reltuples < 0:
			r.unknownFact(s, "rows of "+rel.name, "never analyzed",
				"a NOT NULL column without a DEFAULT fails on a table with rows",
				"ANALYZE "+rel.name)
		}
	}
	r.publishedFinding(s, rel, changereview.SeverityInfo,
		"the new column "+c.Name+" must exist on subscribers before rows carry it.")
	if formatType(c.Type) == "timestamp without time zone" {
		r.find(RuleNewObject, changereview.SeverityInfo, "timestamp without time zone",
			"Column "+c.Name+" stores timestamp without time zone; timestamptz keeps the "+
				"instant whatever the session time zone.", s.evID)
	}
}

// setNotNull is SET NOT NULL: CR-05 when pg_stats shows NULLs, CR-17 when
// the null fraction cannot be read.
func (r *run) setNotNull(s *stmt, i int, rel *relation, expandID string) {
	col := s.parts[i].Column.Name
	frac, ok, err := r.nullFraction(rel, col)
	if r.readFailed(err, s, "null fraction of "+rel.name+"."+col) {
		return
	}
	if !ok {
		r.unknownFact(s, "null fraction of "+rel.name+"."+col, "null fraction unavailable",
			"pg_stats shows no row for the column", "ANALYZE "+rel.name+
				" and grant pg_sage's role SELECT on it")
		return
	}
	if frac <= 0 {
		return
	}
	d := nullsData{Relation: rel.name, Column: col, NullFrac: frac, RelTuples: rel.reltuples,
		EstimatedNullRows: frac * max(rel.reltuples, 0)}
	id := r.addEvidence("nulls", rel.name+"."+col, fmt.Sprintf("%s.%s: null_frac %.4f",
		rel.name, col, frac), d)
	if fill := r.earlierBackfill(s, rel.name, col); fill != nil {
		r.find(RuleWillFail, changereview.SeverityWarning, "SET NOT NULL on "+col+
			" fails unless the backfill fills every NULL", fmt.Sprintf("pg_stats says "+
			"%.2f%% of %s.%s is NULL (about %.0f rows), but statement %d backfills %s "+
			"first; SET NOT NULL fails if any NULL is left.", frac*100, rel.name, col,
			d.EstimatedNullRows, fill.index, col), s.evID, rel.evID, id, fill.evID,
			expandID)
		return
	}
	r.find(RuleWillFail, changereview.SeverityCritical, "SET NOT NULL on "+col+" will fail",
		fmt.Sprintf("pg_stats says %.2f%% of %s.%s is NULL (about %.0f rows): SET NOT NULL "+
			"fails. Backfill first, then add a NOT VALID check, validate it and set NOT "+
			"NULL.", frac*100, rel.name, col, d.EstimatedNullRows), s.evID, rel.evID, id,
		expandID)
}

// earlierBackfill is the statement before s that sets col of rel (an
// UPDATE ... SET col = ...), or nil.
func (r *run) earlierBackfill(s *stmt, rel, col string) *stmt {
	for _, o := range r.stmts {
		if o.index >= s.index {
			return nil
		}
		for i, p := range o.parts {
			if p.Kind == sqlast.DDLUpdate && i < len(o.targets) && o.targets[i] == rel &&
				hasString(p.Columns, col) {
				return o
			}
		}
	}
	return nil
}
