package analyzer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/verify"
)

// IndexFloor is the deterministic index-candidate floor (indexes-12,
// optimizer.Floor): it proposes missing indexes for seq-scan-heavy tables
// when the tuning agent is inactive.
type IndexFloor interface {
	Run(ctx context.Context, snap *collector.Snapshot, tables []string,
		skip map[string]bool) optimizer.FloorResult
}

// WithIndexFloor attaches the deterministic index floor. It runs on a
// cycle with no tuning agent, or one whose agent could not consult its
// model (TuningOutput.ModelUnavailable).
func (a *Analyzer) WithIndexFloor(f IndexFloor) { a.indexFloor = f }

// openShapesSQL reads the open missing_index findings and which path
// proposed each.
const openShapesSQL = `/* pg_sage analyzer */ SELECT object_identifier,
	COALESCE(detail->>'source', '')
	FROM sage.findings
	WHERE category = $1 AND status = 'open'`

// runIndexFloor runs the floor when the agent is inactive this cycle and
// returns its findings. A shape with an open finding of the agent is never
// proposed again. With an agent, its kept findings are the open set (it
// re-emits them); without one the open findings are read, and an
// unreadable set skips the floor (it could not tell what is proposed).
func (a *Analyzer) runIndexFloor(ctx context.Context, cur *collector.Snapshot,
	tuned TuningOutput) []Finding {
	if a == nil || a.indexFloor == nil || a.eval == nil || len(a.eval.seqHeavy) == 0 {
		return nil
	}
	agent := a.tuning != nil
	if (agent && !tuned.ModelUnavailable) || cur == nil || !cur.Available("indexes") {
		return nil
	}
	open := shapesOf(tuned.Findings)
	if !agent {
		var err error
		if open, err = a.readOpenShapes(ctx); err != nil {
			a.logFn("WARN", "analyzer: index floor skipped: open index findings unreadable, "+
				"so the shapes already proposed are unknown: %v", err)
			return nil
		}
	}
	skip := map[string]bool{}
	for id, src := range open {
		// Without an agent the floor re-judges its own open shapes, so they
		// refresh or resolve; an agent re-emits every open shape itself.
		if agent || src != optimizer.FloorSource {
			skip[id] = true
		}
	}
	res := a.indexFloor.Run(ctx, cur, a.eval.seqHeavy, skip)
	if !agent {
		a.settleFloor(open, res)
	}
	return floorFindings(res.Admitted, skip)
}

// shapesOf maps the missing_index findings to their source.
func shapesOf(findings []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range findings {
		if f.Category == optimizer.OptimizerCategory {
			src, _ := f.Detail["source"].(string)
			out[f.ObjectIdentifier] = src
		}
	}
	return out
}

func (a *Analyzer) readOpenShapes(ctx context.Context) (map[string]string, error) {
	q := a.floorReader
	if q == nil {
		if a.pool == nil {
			return map[string]string{}, nil
		}
		q = a.catalog()
	}
	rows, err := q.Query(ctx, openShapesSQL, optimizer.OptimizerCategory)
	if err != nil {
		return nil, fmt.Errorf("read open %s findings: %w", optimizer.OptimizerCategory, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, src string
		if err := rows.Scan(&id, &src); err != nil {
			return nil, fmt.Errorf("scan open %s finding: %w", optimizer.OptimizerCategory, err)
		}
		out[id] = src
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open %s findings: %w", optimizer.OptimizerCategory, err)
	}
	return out, nil
}

// settleFloor decides, on a cycle without an agent, which open
// missing_index findings may resolve by absence: only the floor's own on
// tables a completed floor run fully judged. Every other open finding is
// kept, since nothing else re-emits it this cycle.
func (a *Analyzer) settleFloor(open map[string]string, res optimizer.FloorResult) {
	incomplete := map[string]bool{}
	for _, t := range res.Incomplete {
		incomplete[floorTableKey(t)] = true
	}
	var keep []string
	for id, src := range open {
		table, _, _ := strings.Cut(id, "|")
		if !res.Ran || src != optimizer.FloorSource || incomplete[floorTableKey(table)] {
			keep = append(keep, id)
		}
	}
	a.eval.keep(optimizer.OptimizerCategory, keep...)
	if res.Ran {
		a.eval.evaluated(optimizer.OptimizerCategory)
	}
}

// floorTableKey compares a quoted floor table with a finding's unquoted
// "schema.table".
func floorTableKey(t string) string {
	return strings.ToLower(strings.ReplaceAll(t, `"`, ""))
}

// floorFindings are the admitted candidates as findings with
// detail.source; a skipped or repeated shape is dropped. Only a verified
// (HypoPG-measured) candidate keeps executable SQL.
func floorFindings(admitted []optimizer.Admission, skip map[string]bool) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, adm := range admitted {
		f := OptimizerRecommendationFinding(adm.Rec)
		if skip[f.ObjectIdentifier] || seen[f.ObjectIdentifier] {
			continue
		}
		seen[f.ObjectIdentifier] = true
		f.Detail["source"] = optimizer.FloorSource
		if ladder := verify.LadderValue(adm.Rec.Ladder...); ladder != nil {
			f.Detail["ladder"] = ladder
		}
		if adm.Rec.WhatIf != optimizer.WhatIfVerified {
			adviseUnmeasured(&f, adm.Rec)
		}
		out = append(out, f)
	}
	return out
}

// adviseUnmeasured makes an unmeasured floor finding advisory: no
// executable SQL (the DDL stays in detail.ddl for the operator to read).
func adviseUnmeasured(f *Finding, rec optimizer.Recommendation) {
	f.RecommendedSQL, f.RollbackSQL = "", ""
	f.Detail["advisory"] = true
	why := "HypoPG not installed: impact unmeasured"
	if rec.WhatIfReason != "" && rec.WhatIfReason != "HypoPG unavailable" {
		why = "impact unmeasured: " + rec.WhatIfReason
	}
	f.Recommendation = strings.TrimSpace(f.Recommendation + " (" + why + ")")
}

// seqHeavyTables are this cycle's seq_scan_heavy tables, quoted where
// needed, most sequential scans first.
func seqHeavyTables(snap *collector.Snapshot, findings []Finding) []string {
	flagged := map[string]bool{}
	for _, f := range findings {
		if f.Category == "seq_scan_heavy" {
			flagged[f.ObjectIdentifier] = true
		}
	}
	if len(flagged) == 0 || snap == nil {
		return nil
	}
	var tables []collector.TableStats
	for _, t := range snap.Tables {
		if flagged[t.SchemaName+"."+t.RelName] {
			tables = append(tables, t)
		}
	}
	sort.SliceStable(tables, func(i, j int) bool {
		if tables[i].SeqScan != tables[j].SeqScan {
			return tables[i].SeqScan > tables[j].SeqScan
		}
		return tables[i].SchemaName+"."+tables[i].RelName <
			tables[j].SchemaName+"."+tables[j].RelName
	})
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		out = append(out, sqlident.Qualified(t.SchemaName, t.RelName))
	}
	return out
}

// findingTables are the tables of index findings, for the tuner's deferral.
func findingTables(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, OptimizerFindingTable(f))
	}
	return out
}
