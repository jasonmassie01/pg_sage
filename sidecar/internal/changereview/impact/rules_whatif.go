package impact

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/verify"
)

// whatIfMinPct is the improvement the what-if verdict asks for (the
// optimizer's hypopg_min_improvement_pct default).
const whatIfMinPct = 10.0

// planUsers is the workload statements on rel whose cached plans in
// sage.explain_cache name ix ('Index Name').
func (r *run) planUsers(rel *relation, ix index) []int64 {
	var ids []int64
	for _, ws := range r.work.byRelation[rel.name] {
		ids = append(ids, ws.id)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_plan_users v1 */
		SELECT DISTINCT e.queryid FROM sage.explain_cache e
		WHERE e.queryid = ANY($1::int8[])
		  AND jsonb_path_exists(e.plan_json, '$.** ? (@."Index Name" == $n)',
		                        jsonb_build_object('n', $2::text))
		ORDER BY 1`, ids, ix.name)
	if r.readFailed(err, nil, "cached plans using "+ix.name) {
		return nil
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if r.readFailed(err, nil, "cached plans using "+ix.name) {
		return nil
	}
	return users
}

// hypoDDL is a CREATE INDEX HypoPG can create: no name, CONCURRENTLY or
// IF NOT EXISTS.
func hypoDDL(rel *relation, def *sqlast.IndexDef) string {
	var b strings.Builder
	b.WriteString("CREATE INDEX ON " + pgx.Identifier{rel.schema, rel.rel}.Sanitize())
	if def.Method != "" {
		b.WriteString(" USING " + def.Method)
	}
	b.WriteString(" (" + strings.Join(quoteColumns(def.Columns), ", ") + ")")
	if len(def.Include) > 0 {
		b.WriteString(" INCLUDE (" + strings.Join(quoteColumns(def.Include), ", ") + ")")
	}
	if def.Predicate != "" {
		b.WriteString(" WHERE " + def.Predicate)
	}
	return b.String()
}

// quoteColumns quotes plain column names and keeps expressions.
func quoteColumns(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		if plainIdent(c) {
			out[i] = pgx.Identifier{c}.Sanitize()
		} else {
			out[i] = "(" + c + ")"
		}
	}
	return out
}

func plainIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		word := c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c >= 'A' && c <= 'Z'
		if !word {
			return false
		}
	}
	return true
}

// whatIf is CR-09 for a new index: HypoPG over the workload statements on
// the table (and the review's own reads of it), with the other new
// indexes of the review on that table as the baseline.
func (r *run) whatIf(s *stmt, i int, rel *relation) {
	queries, qids := r.whatIfQueries(rel)
	d := r.measureIndex(rel, s.parts[i].Index, queries, qids, r.alongside(s, i, rel))
	if r.err != nil {
		return
	}
	d.Considered = r.workloadReads(rel)
	r.predict(d.PerQueryPct)
	id := r.addEvidence("whatif", partRef(s.index, i), whatIfTitle(d), d)
	r.find(RuleIndexBenefit, changereview.SeverityInfo, whatIfTitle(d), whatIfText(rel, d),
		s.evID, rel.evID, id)
}

// measureIndex measures def on rel over queries with HypoPG; qids are the
// workload queryids among them (a prediction's targets).
func (r *run) measureIndex(rel *relation, def *sqlast.IndexDef,
	queries []optimizer.QueryInfo, qids []int64, alongside []string) whatIfData {
	d := whatIfData{DDL: hypoDDL(rel, def), Method: verify.MethodHypoPG,
		WritesPerS: rel.rate.ins + rel.rate.upd + rel.rate.del}
	switch {
	case !r.e.d.WhatIf.IsAvailable(r.ctx):
		d.Method, d.Reason = verify.MethodNone, "HypoPG unavailable"
		return d
	case len(queries) == 0:
		d.Method, d.Available = verify.MethodNone, true
		d.Reason = "no workload statement reads " + rel.name
		return d
	}
	d.Available, d.QueryIDs = true, qids
	res, err := r.e.d.WhatIf.Validate(r.ctx, optimizer.Recommendation{Table: rel.name,
		DDL: d.DDL, Alongside: alongside}, queries)
	if r.ctx.Err() != nil {
		r.err = r.ctx.Err()
		return d
	}
	d.Measured, d.Failed, d.SizeBytes = res.Measured, res.Failed, res.SizeBytes
	d.Verdict, d.Reason = optimizer.WhatIfVerdict(res, err, whatIfMinPct)
	if err != nil {
		d.Error = err.Error()
	}
	if err == nil && res.Measured > 0 {
		pct := res.Improvement
		d.ImprovementPct = &pct
		d.PerQueryPct = perQueryPct(res.PerQuery, qids)
	}
	return d
}

// perQueryPct is the per-statement figures of the workload queryids (the
// review's own reads, numbered 1..n, predict nothing after landing).
func perQueryPct(per map[int64]float64, qids []int64) map[string]float64 {
	out := map[string]float64{}
	for _, id := range qids {
		if v, ok := per[id]; ok {
			out[strconv.FormatInt(id, 10)] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func whatIfTitle(d whatIfData) string {
	if d.ImprovementPct == nil {
		return "Index benefit not measured"
	}
	return fmt.Sprintf("HypoPG: %.1f%% lower planned cost for %d statements",
		*d.ImprovementPct, d.Measured)
}

func whatIfText(rel *relation, d whatIfData) string {
	var t string
	switch {
	case !d.Available:
		return "HypoPG unavailable: the index's benefit to the workload was not " +
			"measured; install the hypopg extension to measure it."
	case d.Method == verify.MethodNone:
		return "No workload statement reads " + rel.name + ", so the index's benefit is " +
			"not measured; it adds write cost from the start."
	case d.ImprovementPct == nil:
		t = "HypoPG could not measure the index"
	default:
		t = fmt.Sprintf("With the index, HypoPG plans %d workload statements at %.1f%% "+
			"lower cost (call-weighted; verdict %s)", d.Measured, *d.ImprovementPct,
			d.Verdict)
	}
	if d.Reason != "" {
		t += ": " + d.Reason
	}
	if d.Considered > len(d.QueryIDs) {
		t += fmt.Sprintf(" (the busiest %d of %d workload reads, one per shape)",
			len(d.QueryIDs), d.Considered)
	}
	return t + fmt.Sprintf(". Each of %s's %.2f writes/s also maintains it (%d bytes "+
		"estimated).", rel.name, d.WritesPerS, d.SizeBytes)
}

// maxWhatIfQueries bounds the workload reads one what-if plans (twice
// each, without and with the index): the busiest, one per fingerprint,
// as the optimizer's own what-ifs do.
const maxWhatIfQueries = 50

// whatIfQueries is the read statements on rel: the busiest workload reads
// (at most maxWhatIfQueries, one per fingerprint), then the review's own
// reads (synthetic ids 1..n).
func (r *run) whatIfQueries(rel *relation) ([]optimizer.QueryInfo, []int64) {
	var reads []*workStmt
	for _, ws := range r.work.byRelation[rel.name] {
		if ws.refs.Kind == sqlast.StmtSelect {
			reads = append(reads, ws)
		}
	}
	sort.SliceStable(reads, func(i, j int) bool { return reads[i].calls > reads[j].calls })
	var qs []optimizer.QueryInfo
	var ids []int64
	shapes := map[string]bool{}
	for _, ws := range reads {
		if len(ids) == maxWhatIfQueries {
			break
		}
		if ws.fingerprint != "" && shapes[ws.fingerprint] {
			continue
		}
		shapes[ws.fingerprint] = true
		qs = append(qs, optimizer.QueryInfo{QueryID: ws.id, Text: ws.text, Calls: ws.calls})
		ids = append(ids, ws.id)
	}
	for _, s := range r.stmts {
		if s.kind == "select" && hasString(s.relations, rel.name) {
			qs = append(qs, optimizer.QueryInfo{QueryID: int64(s.index), Text: s.sql,
				Calls: 1})
		}
	}
	return qs, ids
}

// workloadReads is how many workload reads of rel there are.
func (r *run) workloadReads(rel *relation) int {
	n := 0
	for _, ws := range r.work.byRelation[rel.name] {
		if ws.refs.Kind == sqlast.StmtSelect {
			n++
		}
	}
	return n
}

// alongside is the review's other new indexes on rel, as HypoPG DDL.
func (r *run) alongside(s *stmt, i int, rel *relation) []string {
	var out []string
	for _, o := range r.stmts {
		for j, p := range o.parts {
			if (o == s && j == i) || p.Kind != sqlast.DDLCreateIndex || p.Index == nil ||
				j >= len(o.targets) || o.targets[j] != rel.name {
				continue
			}
			out = append(out, hypoDDL(rel, p.Index))
		}
	}
	return out
}
