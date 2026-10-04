package tuning

import (
	"context"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// maxToolQueryChars bounds a statement's text in a tool result.
const maxToolQueryChars = 200

// statement: a workload statement's counters over the interval (never
// cumulative-since-reset totals, which are not current evidence).
func (tb *toolbox) statement(args toolArgs) toolResult {
	qid := int64(args.QueryID)
	if !tb.w.IsWorkload(qid) {
		return refused("queryid %d is not a workload statement of this snapshot", qid)
	}
	var q collector.QueryStats
	for _, s := range tb.cur.Queries {
		if s.QueryID == qid {
			q = s
		}
	}
	iv := delta(q, priorCounters(tb.cur, tb.prev))
	fields := map[string]any{"queryid": fmt.Sprint(qid), "class": tb.w.Statements[qid].Class,
		"interval": map[string]any{"calls": iv.Calls, "total_ms": iv.TotalMs,
			"mean_ms": iv.MeanMs, "rows": iv.Rows, "shared_blks_read": iv.SharedBlksRead,
			"shared_blks_hit": iv.SharedBlksHit, "temp_blks_written": iv.TempBlksWritten,
			"windowed": iv.Windowed}}
	if !slices.ContainsFunc(tb.c.Statements, func(s CaseStatement) bool {
		return s.QueryID == qid
	}) {
		// A case statement's text is in the packet already.
		fields["query"] = clip(oneLine(llm.SanitizeForLLM(q.Query)), maxToolQueryChars)
	}
	return toolResult{ref: fmt.Sprintf("queryid:%d", qid), fields: fields,
		text: fmt.Sprintf("%d calls, %.0f ms in the interval", iv.Calls, iv.TotalMs)}
}

// caseTable resolves a table argument to a workload table of the case.
func (tb *toolbox) caseTable(ref string) (string, error) {
	name := canonicalRef(ref)
	if name == "" {
		return "", fmt.Errorf("%q is not a schema-qualified table", ref)
	}
	if !slices.Contains(tb.c.Tables, name) {
		return "", fmt.Errorf("%s is not a table of case %s", name, tb.c.ID)
	}
	if info := tb.w.Tables[name]; info.Class != ClassApp && info.Class != ClassTenant {
		return "", fmt.Errorf("%s is not workload", name)
	}
	return name, nil
}

// table: the case table's shape, activity and workload hints.
func (tb *toolbox) table(ctx context.Context, args toolArgs) toolResult {
	name, err := tb.caseTable(args.Table)
	if err != nil {
		return toolResult{err: err}
	}
	ts, _ := findSnapshotTable(tb.cur, name)
	fields := map[string]any{"table": name, "live_tuples": ts.NLiveTup,
		"dead_tuples": ts.NDeadTup, "table_bytes": ts.TableBytes,
		"index_bytes": ts.IndexBytes, "seq_scans": ts.SeqScan, "index_scans": ts.IdxScan,
		"inserts": ts.NTupIns, "updates": ts.NTupUpd, "hot_updates": ts.NTupHotUpd,
		"deletes": ts.NTupDel, "storage_parameters": reloptions(tb.cur, ts),
		"indexes": tableIndexes(tb.cur, ts)}
	if tc, ok := tb.a.tableContext(ctx, tb.cur, name); ok {
		fields["columns"] = tc.Columns
		fields["workload_hints"] = workloadHints(tc)
	}
	return toolResult{fields: fields, ref: name,
		text: fmt.Sprintf("%d live rows, %d dead", ts.NLiveTup, ts.NDeadTup)}
}

func tableIndexes(cur *collector.Snapshot, ts collector.TableStats) []map[string]any {
	var out []map[string]any
	for _, ix := range cur.Indexes {
		if ix.SchemaName == ts.SchemaName && ix.RelName == ts.RelName {
			out = append(out, map[string]any{"name": ix.IndexRelName,
				"definition": llm.SanitizeForLLM(ix.IndexDef), "scans": ix.IdxScan,
				"bytes": ix.IndexBytes, "unique": ix.IsUnique, "valid": ix.IsValid})
		}
	}
	return out
}

// workloadHints are the optimizer's deterministic detectors on the
// table's statements: JSON, vector and PostGIS workload shapes, joins,
// INCLUDE and partial-index candidates (filter values left out).
func workloadHints(tc optimizer.TableContext) []string {
	var out []string
	for _, q := range tc.Queries {
		if c := optimizer.ClassifyJSONWorkload(q); c.Shape != "" {
			out = append(out, fmt.Sprintf("queryid %d: JSON %s, consider %s", q.QueryID,
				c.Shape, c.PrimaryRecommendation))
		}
		if c := optimizer.ClassifyVectorWorkload(q); c.Shape != "" {
			out = append(out, fmt.Sprintf("queryid %d: vector %s, consider %s", q.QueryID,
				c.Shape, c.PrimaryRecommendation))
		}
		if c := optimizer.ClassifyPostGISWorkload(q); c.Shape != "" {
			out = append(out, fmt.Sprintf("queryid %d: PostGIS %s, consider %s", q.QueryID,
				c.Shape, c.PrimaryRecommendation))
		}
	}
	for _, jp := range tc.JoinPairs {
		out = append(out, fmt.Sprintf("join %s with %s on %s", jp.Left, jp.Right,
			llm.SanitizeForLLM(jp.Condition)))
	}
	for _, ic := range optimizer.DetectIncludeCandidates(tc.Plans, 1000) {
		out = append(out, fmt.Sprintf("queryid %d: %d heap fetches after an index scan "+
			"(an INCLUDE column may help)", ic.QueryID, ic.HeapFetches))
	}
	for _, pc := range optimizer.DetectPartialCandidates(tc.Queries, tc.ColStats) {
		out = append(out, fmt.Sprintf("%.0f%% of statements filter %s on one value "+
			"matching %.1f%% of rows (a partial index may help)", pc.QueryPct*100,
			pc.Column, pc.Selectivity*100))
	}
	return out
}

// explain: the statement's plan.
func (tb *toolbox) explain(ctx context.Context, args toolArgs) toolResult {
	qid := int64(args.QueryID)
	if !tb.w.IsWorkload(qid) {
		return refused("queryid %d is not a workload statement of this snapshot", qid)
	}
	text := ""
	for _, q := range tb.cur.Queries {
		if q.QueryID == qid {
			text = q.Query
		}
	}
	plan, err := tb.a.deps.Store.Plan(ctx, qid, text)
	if err != nil {
		return refused("no plan for queryid %d: %v", qid, err)
	}
	if plan.Source == PlanSourceNone || len(plan.JSON) == 0 {
		return toolResult{plain: true, fields: map[string]any{"source": PlanSourceNone,
			"note": "no plan is available for this statement"}}
	}
	return toolResult{ref: fmt.Sprintf("queryid:%d", qid),
		text: "plan from " + plan.Source,
		fields: map[string]any{"queryid": fmt.Sprint(qid), "source": plan.Source,
			"plan": clip(llm.SanitizeForLLM(string(plan.JSON)), maxPlanChars)}}
}
