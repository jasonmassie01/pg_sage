package impact

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// concurrentInTransaction is CR-05 for CREATE or DROP INDEX CONCURRENTLY
// inside a transaction block or a transactional migration file.
func (r *run) concurrentInTransaction(s *stmt, i int) {
	p := s.parts[i]
	fileTxn := r.transactional(s)
	if p.Index == nil || !p.Index.Concurrent || (!p.InTransactionBlock && !fileTxn) {
		return
	}
	d := txnData{InTransactionBlock: p.InTransactionBlock,
		TransactionalFile: fileTxn, TransactionalFrom: r.in.TransactionalFrom,
		MigrationTool: r.in.MigrationTool}
	id := r.addEvidence("txn", fmt.Sprint(s.index), "transaction of statement "+
		fmt.Sprint(s.index), d)
	where := "inside the script's BEGIN ... COMMIT"
	if !p.InTransactionBlock {
		where = "in a migration file that runs in a transaction"
		if r.in.MigrationTool != "" {
			where += " (" + r.in.MigrationTool + " wraps each migration in one" +
				fromRequest(r.in.TransactionalFrom) + ")"
		}
	}
	verb := "CREATE INDEX"
	if p.Kind == sqlast.DDLDropIndex {
		verb = "DROP INDEX"
	}
	r.find(RuleWillFail, changereview.SeverityCritical, verb+
		" CONCURRENTLY will fail in a transaction", "CONCURRENTLY cannot run inside a "+
		"transaction block, and this statement runs "+where+": the migration fails. Run "+
		"it in its own non-transactional migration.", s.evID, id)
}

func fromRequest(from string) string {
	if from == changereview.TransactionalFromRequest {
		return "; the request says so"
	}
	return " by default"
}

// dropIndex is CR-07 (and CR-05 for a constraint's index) for DROP INDEX.
func (r *run) dropIndex(s *stmt, i int, rel *relation) {
	p := s.parts[i]
	ix, ok := rel.index(p.Object.Name)
	if !ok {
		if !p.IfExists {
			r.unknownFact(s, "index "+qualified(p.Object), "not found", "", "")
		}
		return
	}
	d := r.indexEvidence(rel, ix)
	id := r.addEvidence("index", d.Index, fmt.Sprintf("%s: %d scans", d.Index, ix.scans), d)
	ids := []string{s.evID, rel.evID, id}
	for _, q := range d.PlanUsers {
		if ws := r.workStatement(q); ws != nil {
			ids = append(ids, r.queryEvidence(ws, rel))
		}
	}
	sev, title := indexUseSeverity(d)
	r.find(RuleIndexUsed, sev, title, indexUseText(d), ids...)
	if len(ix.backing) > 0 && p.Behavior != "cascade" {
		r.find(RuleWillFail, changereview.SeverityCritical, "Dropping "+d.Index+" will fail",
			"PostgreSQL refuses to drop "+d.Index+": constraint "+strings.Join(ix.backing,
				", ")+" requires it. Drop the constraint instead.", s.evID, id)
	}
}

func indexUseSeverity(d indexData) (string, string) {
	switch {
	case len(d.ConstraintBacked) > 0:
		return changereview.SeverityCritical, d.Index + " is constraint-backed"
	case d.Scans > 0:
		return changereview.SeverityCritical, fmt.Sprintf("%s is still used: %d scans",
			d.Index, d.Scans)
	case len(d.PlanUsers) > 0 || len(d.SupportsFKs) > 0:
		return changereview.SeverityWarning, d.Index + " is still planned or supports a " +
			"foreign key"
	}
	return changereview.SeverityInfo, d.Index + " was never scanned"
}

func indexUseText(d indexData) string {
	t := fmt.Sprintf("%s (%s) has %d scans since the statistics were reset", d.Index,
		d.Definition, d.Scans)
	if d.LastScan != nil {
		t += ", the last at " + d.LastScan.UTC().Format("2006-01-02 15:04:05Z")
	}
	t += "."
	if len(d.ConstraintBacked) > 0 {
		t += " Constraints " + strings.Join(d.ConstraintBacked, ", ") + " require it."
	}
	if len(d.PlanUsers) > 0 {
		t += fmt.Sprintf(" %d cached plans use it (queryids %s).", len(d.PlanUsers),
			joinIDs(d.PlanUsers))
	}
	if len(d.SupportsFKs) > 0 {
		t += " It supports foreign keys " + strings.Join(d.SupportsFKs, ", ") +
			": without it, deletes on the referenced table scan this one."
	}
	return t
}

// indexEvidence is an existing index's facts, with its plan users.
func (r *run) indexEvidence(rel *relation, ix index) indexData {
	d := indexData{Index: rel.schema + "." + ix.name, Relation: rel.name, Definition: ix.def,
		Unique: ix.unique, Primary: ix.primary, Valid: ix.valid, Scans: ix.scans,
		LastScan: ix.lastScan, ConstraintBacked: ix.backing, PlanUsers: r.planUsers(rel, ix)}
	for _, c := range rel.constraints {
		if c.typ == "f" && leading(ix.parsed.Columns, c.columns) {
			d.SupportsFKs = append(d.SupportsFKs, c.name)
		}
	}
	return d
}

// leading reports whether cols are the leading columns of key (any order).
func leading(key, cols []string) bool {
	if len(cols) == 0 || len(cols) > len(key) {
		return false
	}
	for _, c := range cols {
		if !hasString(key[:len(cols)], c) {
			return false
		}
	}
	return true
}

func (rel *relation) index(name string) (index, bool) {
	for _, ix := range rel.indexes {
		if ix.name == name {
			return ix, true
		}
	}
	return index{}, false
}

func (r *run) workStatement(id int64) *workStmt {
	for _, ws := range r.work.stmts {
		if ws.id == id {
			return ws
		}
	}
	return nil
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ", ")
}

// newIndex is a CREATE INDEX as the index parser describes indexes.
func newIndex(rel *relation, def *sqlast.IndexDef) analyzer.ParsedIndex {
	return analyzer.ParsedIndex{Schema: rel.schema, Table: rel.rel, Name: def.Name,
		Columns: def.Columns, IncludeCols: def.Include,
		WhereClause: unparen(def.Predicate), IndexType: def.Method}
}

func unparen(s string) string {
	s = strings.TrimSpace(s)
	for len(s) > 1 && s[0] == '(' && s[len(s)-1] == ')' {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// redundantIndexes is CR-08 for a new index: a duplicate or a prefix of an
// existing one (warning), or a wider index that makes one redundant (info).
func (r *run) redundantIndexes(s *stmt, i int, rel *relation) {
	cand := newIndex(rel, s.parts[i].Index)
	for _, ix := range rel.indexes {
		old := ix.parsed
		old.WhereClause = unparen(old.WhereClause)
		if old.IndexType != cand.IndexType {
			continue
		}
		name := rel.schema + "." + ix.name
		id := func() string {
			return r.addEvidence("index", name, name+": "+ix.def, r.indexEvidence(rel, ix))
		}
		switch {
		case analyzer.IsDuplicate(cand, old):
			r.find(RuleIndexRedundant, changereview.SeverityWarning, "The new index "+
				"duplicates "+name, "The new index has the same columns as "+name+" ("+
				ix.def+"): it doubles write cost and adds nothing.", s.evID, id())
		case analyzer.IsSubset(cand, old):
			r.find(RuleIndexRedundant, changereview.SeverityWarning, "The new index is "+
				"covered by "+name, "The new index's columns lead "+name+" ("+ix.def+
				"), which already serves its lookups.", s.evID, id())
		case analyzer.IsSubset(old, cand) && !ix.unique:
			r.find(RuleIndexRedundant, changereview.SeverityInfo, name+" becomes redundant",
				"The new index starts with "+name+"'s columns, so "+name+" becomes "+
					"redundant once it is valid; drop it in a later change.", s.evID, id())
		}
	}
}
