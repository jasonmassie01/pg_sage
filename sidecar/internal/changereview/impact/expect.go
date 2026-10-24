package impact

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/verify"
)

// constraintValidated in a constraint post-condition's Type asks for a
// validated constraint (VALIDATE CONSTRAINT).
const constraintValidated = "validated"

// predictionClass labels Change Review's predictions.
const predictionClass = "change_review"

// postCondition is what the catalog shows once part p of statement n has
// landed. indexTable is the table of a dropped index ("" when unknown;
// the index's own schema then locates it). ok is false for statements
// that are not DDL, settings, SET DEFAULT and CREATE STATISTICS.
func postCondition(n int, p sqlast.DDLStatement, indexTable string) (
	changereview.PostCondition, bool) {
	pc := changereview.PostCondition{StatementIndex: n, Relation: qualified(p.Relation)}
	col := ""
	if p.Column != nil {
		col = p.Column.Name
	} else if len(p.Columns) > 0 {
		col = p.Columns[0]
	}
	switch p.Kind {
	case sqlast.DDLCreateIndex:
		pc.Kind, pc.Index = changereview.PostIndexValid, indexName(p)
	case sqlast.DDLDropIndex:
		pc.Kind, pc.Index, pc.Relation = changereview.PostIndexAbsent, p.Object.Name,
			indexTable
		if pc.Relation == "" {
			pc.Relation = qualified(p.Object)
		}
	case sqlast.DDLAddColumn:
		pc.Kind, pc.Column, pc.Type = changereview.PostColumnPresent, col,
			formatType(p.Column.Type)
	case sqlast.DDLDropColumn:
		pc.Kind, pc.Column = changereview.PostColumnAbsent, col
	case sqlast.DDLRenameColumn:
		pc.Kind, pc.Column, pc.NewName = changereview.PostColumnRenamed, col, p.NewName
	case sqlast.DDLAlterType:
		pc.Kind, pc.Column, pc.Type = changereview.PostColumnType, col,
			formatType(p.Column.Type)
	case sqlast.DDLSetNotNull:
		pc.Kind, pc.Column = changereview.PostNotNull, col
	case sqlast.DDLDropNotNull:
		pc.Kind, pc.Column = changereview.PostNullable, col
	default:
		return relationCondition(pc, p)
	}
	return pc, true
}

func relationCondition(pc changereview.PostCondition, p sqlast.DDLStatement) (
	changereview.PostCondition, bool) {
	switch p.Kind {
	case sqlast.DDLAddConstraint:
		pc.Kind, pc.Constraint = changereview.PostConstraint, constraintName(p)
	case sqlast.DDLValidateConstraint:
		pc.Kind, pc.Constraint, pc.Type = changereview.PostConstraint, p.Constraint.Name,
			constraintValidated
	case sqlast.DDLDropConstraint:
		pc.Kind, pc.Constraint = changereview.PostConstraintAbsent, p.Constraint.Name
	case sqlast.DDLCreateTable:
		pc.Kind = changereview.PostRelationPresent
	case sqlast.DDLDropTable:
		pc.Kind = changereview.PostRelationAbsent
	case sqlast.DDLRenameTable:
		pc.Kind, pc.NewName = changereview.PostRelationRenamed, p.NewName
	default:
		return changereview.PostCondition{}, false
	}
	return pc, pc.Relation != ""
}

// indexName is a new index's name, or the one PostgreSQL generates:
// table_col1_col2_idx ("expr" for an expression), shortened to fit.
func indexName(p sqlast.DDLStatement) string {
	if p.Index != nil && p.Index.Name != "" {
		return p.Index.Name
	}
	var cols []string
	if p.Index != nil {
		for _, c := range p.Index.Columns {
			if !plainIdent(c) {
				c = "expr"
			}
			cols = append(cols, c)
		}
	}
	return objectName(p.Relation.Name, strings.Join(cols, "_"), "idx")
}

// maxNameBytes is NAMEDATALEN - 1: the longest identifier PostgreSQL keeps.
const maxNameBytes = 63

// objectName is PostgreSQL's makeObjectName: name1_name2_label, the
// longer of name1 and name2 shortened a byte at a time (name2 on a tie)
// until the whole fits in 63 bytes, never splitting a character.
func objectName(name1, name2, label string) string {
	overhead := len(label) + 1
	if name2 != "" {
		overhead++
	}
	n1, n2 := len(name1), len(name2)
	for n1+n2 > maxNameBytes-overhead {
		if n1 > n2 {
			n1--
		} else {
			n2--
		}
	}
	out := clipBytes(name1, n1)
	if name2 != "" {
		out += "_" + clipBytes(name2, n2)
	}
	return out + "_" + label
}

// clipBytes is the longest prefix of s of at most n bytes that ends on a
// character boundary (pg_mbcliplen).
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// constraintSuffixes are PostgreSQL's generated constraint name endings.
var constraintSuffixes = map[string]string{sqlast.ConstraintForeignKey: "fkey",
	sqlast.ConstraintUnique: "key", sqlast.ConstraintCheck: "check",
	sqlast.ConstraintExclusion: "excl"}

// constraintName is a new constraint's name, or the one PostgreSQL
// generates (table_pkey, table_col_fkey, table_col_key, table_col_check;
// a CHECK over no column or several is table_check).
func constraintName(p sqlast.DDLStatement) string {
	c := p.Constraint
	if c == nil {
		return ""
	}
	if c.Name != "" {
		return c.Name
	}
	if c.Kind == sqlast.ConstraintPrimaryKey {
		return objectName(p.Relation.Name, "", "pkey")
	}
	cols := strings.Join(c.Columns, "_")
	if c.Kind == sqlast.ConstraintCheck && len(c.Columns) != 1 {
		cols = ""
	}
	return objectName(p.Relation.Name, cols, constraintSuffixes[c.Kind])
}

// predict records each CR-09 target's own HypoPG-predicted change (a
// cost reduction is a negative expected change), by queryid.
func (r *run) predict(per map[string]float64) {
	if r.predicted == nil {
		r.predicted = map[int64]float64{}
	}
	for key, pct := range per {
		if id, err := strconv.ParseInt(key, 10, 64); err == nil {
			r.predicted[id] = -pct
		}
	}
}

// expectations are the review's post-conditions, watch set (the cited
// workload statements first, then the busiest), touched relations and a
// prediction per watched statement.
func (r *run) expectations() changereview.Expectations {
	exp := changereview.Expectations{PostConditions: []changereview.PostCondition{},
		WatchQueryIDs: []int64{}, Relations: []string{}, Predictions: []verify.Prediction{}}
	for _, s := range r.stmts {
		for i, p := range s.parts {
			if pc, ok := postCondition(s.index, p, s.targets[i]); ok {
				exp.PostConditions = append(exp.PostConditions, pc)
			}
			if t := s.targets[i]; t != "" && !hasString(exp.Relations, t) {
				exp.Relations = append(exp.Relations, t)
			}
		}
	}
	sort.Strings(exp.Relations)
	exp.WatchQueryIDs = r.watchSet(exp.Relations)
	for _, id := range exp.WatchQueryIDs {
		exp.Predictions = append(exp.Predictions, r.prediction(id))
	}
	return exp
}

func (r *run) prediction(id int64) verify.Prediction {
	pct, method, note := 0.0, verify.MethodRule, "no change expected"
	if p, ok := r.predicted[id]; ok {
		pct, method, note = p, verify.MethodHypoPG, "HypoPG what-if of the new index"
	}
	return verify.Prediction{Class: predictionClass, Method: method,
		Metric: verify.MetricMeanExecTime, TargetQueryIDs: []int64{id},
		ExpectedChangePct: &pct, Source: "impact", Note: note}
}

// watchSet is at most MaxWatchQueryIDs workload statements on rels.
func (r *run) watchSet(rels []string) []int64 {
	cited := map[int64]bool{}
	for _, f := range r.finds {
		for _, id := range f.EvidenceIDs {
			if ws := r.workStatementByEvidence(id); ws != nil {
				cited[ws.id] = true
			}
		}
	}
	var cands []*workStmt
	seen := map[int64]bool{}
	for _, rel := range rels {
		for _, ws := range r.work.byRelation[rel] {
			if !seen[ws.id] {
				seen[ws.id] = true
				cands = append(cands, ws)
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cited[cands[i].id] != cited[cands[j].id] {
			return cited[cands[i].id]
		}
		if cands[i].calls != cands[j].calls {
			return cands[i].calls > cands[j].calls
		}
		return cands[i].id < cands[j].id
	})
	out := []int64{}
	for _, ws := range cands[:min(len(cands), changereview.MaxWatchQueryIDs)] {
		out = append(out, ws.id)
	}
	return out
}

func (r *run) workStatementByEvidence(id string) *workStmt {
	if !strings.HasPrefix(id, "queryid:") {
		return nil
	}
	for _, ws := range r.work.stmts {
		if ws.evID == id {
			return ws
		}
	}
	return nil
}
