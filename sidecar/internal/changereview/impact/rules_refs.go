package impact

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// referenced is what still uses a name a statement removes: workload
// statements (with the ones that read every column or an ambiguous name)
// and function bodies.
type referenced struct {
	stmts     []*workStmt
	star      []*workStmt
	ambiguous []*workStmt
	known     bool
}

// columnUsers is the workload statements using rel.col; known is false
// when the workload could not be read or hides statement text.
func (r *run) columnUsers(s *stmt, rel *relation, col string) referenced {
	out := referenced{known: r.workloadKnown(s, rel.name+"."+col)}
	for _, ws := range r.work.byRelation[rel.name] {
		switch {
		case hasString(columnsOf(ws.refs, rel), col):
			out.stmts = append(out.stmts, ws)
		case readsStar(ws.refs, rel):
			out.star = append(out.star, ws)
		case ambiguousOn(ws.refs, rel, col):
			out.ambiguous = append(out.ambiguous, ws)
		}
	}
	return out
}

// workloadKnown reports whether the workload shows every statement on
// subject; otherwise it records CR-17 for subject and returns false.
func (r *run) workloadKnown(s *stmt, subject string) bool {
	if !r.work.available {
		r.unknownFact(s, "workload using "+subject, r.work.reason, "", r.work.fix)
		return false
	}
	if r.work.hidden > 0 {
		r.unknownFact(s, "workload using "+subject, "query text hidden",
			fmt.Sprintf("%d pg_stat_statements entries show %s", r.work.hidden,
				hiddenQueryText), "grant pg_sage's role pg_read_all_stats")
		return false
	}
	return true
}

// referenceFindings raises CR-03 or CR-04 for the statements using name,
// critical when any of them ran, plus CR-17 for SELECT * and ambiguous
// names.
func (r *run) referenceFindings(s *stmt, rule string, rel *relation, name, verb string,
	ref referenced) {
	if len(ref.stmts) > 0 {
		ids := []string{s.evID, rel.evID}
		var calls int64
		var parts []string
		for _, ws := range ref.stmts {
			ids = append(ids, r.queryEvidence(ws, rel))
			calls += ws.calls
			parts = append(parts, describeStatement(ws))
		}
		r.find(rule, severityFor(calls > 0), fmt.Sprintf("%s is %s but %d workload "+
			"statements still use it", name, verb, len(ref.stmts)),
			fmt.Sprintf("%s is %s while %d statements still use it (%d calls): %s. Ship "+
				"the code that stops using it first (expand, then contract).", name, verb,
				len(ref.stmts), calls, strings.Join(parts, "; ")), ids...)
	}
	if len(ref.star) > 0 {
		r.ambiguousFinding(s, rel, ref.star, name, "reads every column",
			"SELECT * over "+rel.name+" reads every column, so which statements read "+name+
				" is not known")
	}
	if len(ref.ambiguous) > 0 {
		r.ambiguousFinding(s, rel, ref.ambiguous, name, "ambiguous column",
			"an unqualified "+name+" over a join that includes "+rel.name+
				" may be this column")
	}
}

func (r *run) ambiguousFinding(s *stmt, rel *relation, stmts []*workStmt, name, reason,
	detail string) {
	var ids []string
	for _, ws := range stmts {
		ids = append(ids, r.queryEvidence(ws, rel))
	}
	r.unknownFact(s, "statements using "+name, reason,
		fmt.Sprintf("%s; %d statements", detail, len(stmts)), "", ids...)
}

func describeStatement(ws *workStmt) string {
	d := fmt.Sprintf("queryid %d (%d calls", ws.id, ws.calls)
	if ws.rateKnown {
		d += fmt.Sprintf(", %d in 7 days", ws.rate.calls)
	}
	d += ")"
	for _, k := range []string{"application", "route", "controller", "action"} {
		if v := ws.tags[k]; v != "" {
			d += " " + k + "=" + v
		}
	}
	return d
}

// functionsNaming is the functions whose bodies name any of names as a
// whole word (SQL and PL bodies; C functions have no body to read).
func (r *run) functionsNaming(s *stmt, names ...string) []functionData {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_functions v1 */
		SELECT n.nspname || '.' || p.proname, l.lanname
		FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_catalog.pg_language l ON l.oid = p.prolang
		WHERE l.lanname NOT IN ('c', 'internal')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
		  AND n.nspname !~ '^pg_toast' AND p.prosrc ~* $1
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
		        WHERE d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = p.oid
		          AND d.deptype = 'e')
		ORDER BY 1 LIMIT 50`, wordPattern(names))
	if r.readFailed(err, s, "function bodies naming "+strings.Join(names, ", ")) {
		return nil
	}
	fns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (functionData, error) {
		f := functionData{Names: names}
		return f, row.Scan(&f.Function, &f.Language)
	})
	if r.readFailed(err, s, "function bodies naming "+strings.Join(names, ", ")) {
		return nil
	}
	return fns
}

// functionFindings raises rule as a warning naming the functions.
func (r *run) functionFindings(s *stmt, rule, name, verb string, fns []functionData) {
	if len(fns) == 0 {
		return
	}
	ids := []string{s.evID}
	var list []string
	for _, f := range fns {
		ids = append(ids, r.addEvidence("function", f.Function, f.Function+" ("+f.Language+
			") names "+name, f))
		list = append(list, f.Function)
	}
	r.find(rule, changereview.SeverityWarning, fmt.Sprintf("%s is %s but function "+
		"bodies name it: %s", name, verb, strings.Join(list, ", ")),
		fmt.Sprintf("The bodies of %s mention %s; they fail at run time once it is %s. "+
			"Update them in the same change.", strings.Join(list, ", "), name, verb), ids...)
}

// publishedFinding raises CR-13 for a change to a published table.
func (r *run) publishedFinding(s *stmt, rel *relation, severity, what string) {
	if len(rel.publications) == 0 {
		return
	}
	ids := []string{s.evID, rel.evID}
	for _, p := range rel.publications {
		ids = append(ids, r.addEvidence("publication", p, p+" publishes "+rel.name,
			publicationData{Publication: p, Relation: rel.name}))
	}
	pubs := strings.Join(rel.publications, ", ")
	r.find(RulePublished, severity, rel.name+" is published ("+pubs+")",
		fmt.Sprintf("%s is in logical-replication publication %s: %s Subscribers apply "+
			"rows by column; change them in step.", rel.name, pubs, what), ids...)
}
