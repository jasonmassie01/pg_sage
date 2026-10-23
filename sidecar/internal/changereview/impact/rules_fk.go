package impact

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// foreignKeyRules is CR-10 and CR-11 for every foreign key part i adds.
func (r *run) foreignKeyRules(s *stmt, i int, rel *relation) {
	p := s.parts[i]
	for k, fk := range foreignKeys(&p) {
		ref := r.rels[qualified(*fk.def.RefRelation)]
		if ref == nil || (!ref.found && !ref.created) {
			r.unknownFact(s, "referenced table "+qualified(*fk.def.RefRelation),
				"not found", "", "")
			continue
		}
		d := fkData{Relation: rel.name, Columns: fk.columns, RefRelation: ref.name,
			RefColumns: fk.def.RefColumns, RateKnown: ref.rate.known,
			RefUpdPerS: ref.rate.upd, RefDelPerS: ref.rate.del}
		if len(d.RefColumns) == 0 {
			d.RefColumns = ref.primaryKey()
		}
		d.Types = columnTypes(rel, &p, d.Columns)
		d.RefTypes = columnTypes(ref, nil, d.RefColumns)
		d.CoveringIndex = r.coveringIndex(rel, d.Columns)
		id := r.addEvidence("fk", fmt.Sprintf("%s.%d", partRef(s.index, i), k),
			fmt.Sprintf("%s(%s) references %s", rel.name, strings.Join(d.Columns, ", "),
				ref.name), d)
		r.fkIndexFinding(s, rel, d, id)
		r.fkTypeFinding(s, d, id)
	}
}

func (r *run) fkIndexFinding(s *stmt, rel *relation, d fkData, id string) {
	if d.CoveringIndex != "" {
		return
	}
	sev := changereview.SeverityWarning
	if rel.created {
		sev = changereview.SeverityInfo
	}
	rate := "its write rate is unknown (no tables snapshot an hour old)"
	if d.RateKnown {
		rate = fmt.Sprintf("it sees %.3f updates/s and %.3f deletes/s", d.RefUpdPerS,
			d.RefDelPerS)
	}
	r.find(RuleFKUnindexed, sev, "No index leads with "+rel.name+"("+
		strings.Join(d.Columns, ", ")+")", fmt.Sprintf("Every delete or key update on "+
		"%s checks %s for referencing rows, and without an index leading with (%s) each "+
		"check scans %s (%.0f rows); %s. Create the index CONCURRENTLY first.",
		d.RefRelation, rel.name, strings.Join(d.Columns, ", "), rel.name, rel.reltuples,
		rate), s.evID, rel.evID, id)
}

func (r *run) fkTypeFinding(s *stmt, d fkData, id string) {
	if len(d.Types) != len(d.RefTypes) {
		return
	}
	var diffs []string
	for i := range d.Types {
		if d.Types[i] != "" && d.RefTypes[i] != "" && d.Types[i] != d.RefTypes[i] {
			diffs = append(diffs, fmt.Sprintf("%s is %s but %s.%s is %s", d.Columns[i],
				d.Types[i], d.RefRelation, d.RefColumns[i], d.RefTypes[i]))
		}
	}
	if len(diffs) == 0 {
		return
	}
	r.find(RuleFKTypeMismatch, changereview.SeverityWarning, "Foreign key column types "+
		"differ", strings.Join(diffs, "; ")+": every check casts, and the referencing "+
		"column overflows before the key does.", s.evID, id)
}

// columnTypes is the format_type of cols of rel, from the catalog or, for
// columns the part itself adds, from its definition ("" when unknown).
func columnTypes(rel *relation, p *sqlast.DDLStatement, cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		if col, ok := rel.columns[c]; ok {
			out[i] = col.typ
		}
		if p == nil {
			continue
		}
		if p.Column != nil && p.Column.Name == c {
			out[i] = formatType(p.Column.Type)
		}
		if p.Table != nil {
			for _, tc := range p.Table.Columns {
				if tc.Name == c {
					out[i] = formatType(tc.Type)
				}
			}
		}
	}
	return out
}

func (rel *relation) primaryKey() []string {
	for _, c := range rel.constraints {
		if c.typ == "p" {
			return c.columns
		}
	}
	return nil
}

// coveringIndex is an index of rel, existing or created by this review,
// whose leading columns are cols.
func (r *run) coveringIndex(rel *relation, cols []string) string {
	for _, ix := range rel.indexes {
		if ix.valid && leading(ix.parsed.Columns, cols) {
			return rel.schema + "." + ix.name
		}
	}
	for _, o := range r.stmts {
		for j, p := range o.parts {
			if p.Kind == sqlast.DDLCreateIndex && p.Index != nil && j < len(o.targets) &&
				o.targets[j] == rel.name && leading(p.Index.Columns, cols) {
				return "new index of statement " + fmt.Sprint(o.index)
			}
		}
	}
	return ""
}
