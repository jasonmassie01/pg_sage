package tuning

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexOnCaseTable parses a single CREATE INDEX on a case table.
func (tb *toolbox) indexOnCaseTable(ddl string) (string, string, error) {
	sql, ok := singleStatement(ddl)
	if !ok {
		return "", "", fmt.Errorf("one CREATE INDEX statement only")
	}
	spec, err := optimizer.ParseIndexDDL(sql)
	if err != nil {
		return "", "", fmt.Errorf("not a CREATE INDEX statement: %v", err)
	}
	if spec.TableSchema == "" {
		return "", "", fmt.Errorf("the table must be schema-qualified")
	}
	table, err := tb.caseTable(qualified(spec.TableSchema, spec.TableName))
	return sql, table, err
}

// whatIf: measure an index with HypoPG through the optimizer's admission
// (validator and rejection memory included), at most maxWhatIfPerCase.
func (tb *toolbox) whatIf(ctx context.Context, args toolArgs) toolResult {
	ddl, table, err := tb.indexOnCaseTable(args.DDL)
	if err != nil {
		return toolResult{err: err}
	}
	if tb.whatIfs >= maxWhatIfPerCase {
		return refused("at most %d what-if measurements per case", maxWhatIfPerCase)
	}
	tc, ok := tb.a.tableContext(ctx, tb.cur, table)
	if !ok {
		return refused("no table context for %s", table)
	}
	tb.whatIfs++
	adm := tb.a.deps.Indexes.Admit(ctx, optimizer.Recommendation{DDL: ddl,
		Category: optimizer.OptimizerCategory}, tc)
	verdict := map[optimizer.AdmissionOutcome]string{optimizer.AdmitInvalid: "invalid",
		optimizer.AdmitMeasured: "already_measured",
		optimizer.AdmitRejected: "rejected"}[adm.Outcome]
	if adm.Outcome == optimizer.AdmitAccepted {
		verdict = adm.Rec.WhatIf
	}
	fields := map[string]any{"ddl": ddl, "verdict": verdict,
		"improvement_pct": adm.Rec.EstimatedImprovementPct,
		"reason":          firstNonEmpty(adm.Reason, adm.Rec.WhatIfReason)}
	if adm.Rec.CostEstimate != nil {
		fields["size_bytes"] = adm.Rec.CostEstimate.EstimatedSizeBytes
	}
	return toolResult{fields: fields, ref: table,
		text: fmt.Sprintf("what-if %s: %.1f%%", verdict, adm.Rec.EstimatedImprovementPct)}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// writeCost: what an index on the columns would cost on writes.
func (tb *toolbox) writeCost(ctx context.Context, args toolArgs) toolResult {
	table, err := tb.caseTable(args.Table)
	if err != nil {
		return toolResult{err: err}
	}
	if len(args.Columns) == 0 {
		return refused("name the index columns")
	}
	schema, rel, _ := splitQualified(table)
	stats, err := tb.a.deps.Store.ColumnStats(ctx, schema, rel, args.Columns)
	if err != nil {
		return refused("column statistics unreadable: %v", err)
	}
	widths := make([]int, 0, len(args.Columns))
	for _, s := range stats {
		for _, col := range args.Columns {
			if s.Column == col {
				widths = append(widths, s.AvgWidth)
			}
		}
	}
	in := tb.writeRates(table)
	in.EntryBytes = entryBytes(widths)
	wc := EstimateWriteCost(in)
	return toolResult{ref: table, text: fmt.Sprintf("%.1f index writes/s for the new "+
		"index", wc.NewIndexWritesPerSec),
		fields: map[string]any{"table": table, "columns": args.Columns,
			"entry_bytes": in.EntryBytes, "existing_indexes": in.Indexes,
			"index_writes_per_sec":       wc.IndexWritesPerSec,
			"new_index_writes_per_sec":   wc.NewIndexWritesPerSec,
			"new_index_bytes_per_sec":    wc.NewIndexBytesPerSec,
			"share_of_index_maintenance": wc.ShareOfIndexMaintenance,
			"hot_ratio":                  wc.HOTRatio,
			"estimated_index_bytes":      wc.EstimatedNewIndexBytes}}
}

// writeRates are a table's write rates over the interval (zero on the
// first cycle) and its index count.
func (tb *toolbox) writeRates(table string) WriteCostInput {
	ts, _ := findSnapshotTable(tb.cur, table)
	in := WriteCostInput{LiveTuples: ts.NLiveTup}
	for _, ix := range tb.cur.Indexes {
		if ix.SchemaName == ts.SchemaName && ix.RelName == ts.RelName {
			in.Indexes++
		}
	}
	prev, ok := findSnapshotTable(tb.prev, table)
	if !ok {
		return in
	}
	secs := tb.cur.CollectedAt.Sub(tb.prev.CollectedAt).Seconds()
	if secs <= 0 {
		return in
	}
	in.InsertsPerSec = float64(ts.NTupIns-prev.NTupIns) / secs
	in.UpdatesPerSec = float64(ts.NTupUpd-prev.NTupUpd) / secs
	in.HotUpdatesPerSec = float64(ts.NTupHotUpd-prev.NTupHotUpd) / secs
	in.DeletesPerSec = float64(ts.NTupDel-prev.NTupDel) / secs
	return in
}

// extendedStats: existing statistics objects and the columns' statistics.
func (tb *toolbox) extendedStats(ctx context.Context, args toolArgs) toolResult {
	table, err := tb.caseTable(args.Table)
	if err != nil {
		return toolResult{err: err}
	}
	schema, rel, _ := splitQualified(table)
	existing, err := tb.a.deps.Store.ExtendedStats(ctx, schema, rel)
	if err != nil {
		return refused("extended statistics unreadable: %v", err)
	}
	cols, err := tb.a.deps.Store.ColumnStats(ctx, schema, rel, args.Columns)
	if err != nil {
		return refused("column statistics unreadable: %v", err)
	}
	return toolResult{ref: table, text: fmt.Sprintf("%d statistics objects", len(existing)),
		fields: map[string]any{"table": table, "existing": existing, "columns": cols}}
}

// rehearse: build the index on a disposable clone and plan the case
// statements before and after; once per cycle.
func (tb *toolbox) rehearse(ctx context.Context, args toolArgs) toolResult {
	if tb.a.deps.Rehearse == nil {
		return refused("no clone provider is configured")
	}
	ddl, table, err := tb.indexOnCaseTable(args.DDL)
	if err != nil {
		return toolResult{err: err}
	}
	tb.cycle.mu.Lock()
	done := tb.cycle.rehearsed
	tb.cycle.rehearsed = true
	tb.cycle.mu.Unlock()
	if done {
		return refused("one rehearsal per cycle has been used")
	}
	req := RehearsalRequest{DDL: ddl}
	for _, s := range tb.c.Statements {
		req.Statements = append(req.Statements, RehearsalStatement{QueryID: s.QueryID,
			Text: s.Text})
	}
	res, err := tb.a.deps.Rehearse.Rehearse(ctx, req)
	if err != nil {
		return refused("rehearsal failed: %v", err)
	}
	return toolResult{ref: table, text: fmt.Sprintf("built in %d ms", res.BuildMs),
		fields: map[string]any{"ddl": ddl, "build_ms": res.BuildMs,
			"size_delta_bytes": res.SizeDeltaBytes, "queries": res.Queries,
			"note": strings.TrimSpace(res.Note)}}
}
