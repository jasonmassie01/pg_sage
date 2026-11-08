package analyzer

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// tempSpillClearIdentities requires positive clearance for each query. Evicted,
// truncated, inactive and reset counters cannot establish recovery. Even a
// sub-threshold new spill keeps an existing finding open.
func tempSpillClearIdentities(cur, prev *collector.Snapshot) []string {
	if !hasTempSpillWindow(cur, prev) {
		return nil
	}
	old := make(map[int64]collector.QueryStats, len(prev.Queries))
	for _, q := range prev.Queries {
		old[q.QueryID] = q
	}
	var ids []string
	for _, q := range adviceQueries(cur) {
		p, ok := old[q.QueryID]
		if ok && q.Calls > p.Calls && q.TempBlksWritten == p.TempBlksWritten && q.TempBlksRead == p.TempBlksRead {
			ids = append(ids, fmt.Sprintf("queryid:%d", q.QueryID))
		}
	}
	return ids
}

func (a *Analyzer) resolveClearedTempSpills(ctx context.Context, cur, prev *collector.Snapshot, active []Finding) {
	ids := tempSpillClearIdentities(cur, prev)
	if len(ids) == 0 {
		return
	}
	current := make(map[string]bool)
	for _, f := range active {
		if f.Category == "temp_spill" {
			current[f.ObjectIdentifier] = true
		}
	}
	clear := ids[:0]
	for _, id := range ids {
		if !current[id] {
			clear = append(clear, id)
		}
	}
	if len(clear) == 0 {
		return
	}
	if _, err := a.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.findings SET status='resolved', resolved_at=now()
	 WHERE category='temp_spill' AND status='open' AND object_identifier=ANY($1::text[])`, clear); err != nil {
		a.logFn("ERROR", "analyzer: resolve observed zero-spill queries: %v", err)
	}
}

func (a *Analyzer) finishTempSpillCycle(ctx context.Context, cur, prev *collector.Snapshot, active []Finding) {
	a.eval.fail("temp_spill") // Never use category-wide absence as recovery.
	a.resolveClearedTempSpills(ctx, cur, prev, active)
}

// Only report new work, not the cumulative residue of an old spill. A missing
// baseline or a reset is unknown, so it must not resolve an existing finding.
func hasTempSpillWindow(cur, prev *collector.Snapshot) bool {
	return cur != nil && prev != nil && cur.Available("queries") && prev.Available("queries") &&
		!cur.StatsReset && cur.CollectedAt.After(prev.CollectedAt) &&
		!cur.StatsEpoch.IsZero() && !prev.StatsEpoch.IsZero() &&
		cur.StatsEpoch.Equal(prev.StatsEpoch)
}

// ruleTempSpills is independent of plan capture and pg_hint_plan. Temp blocks
// establish a spill, not its cause or a safe work_mem value. 128 blocks keeps
// tiny transient files out of the recommendation stream.
func ruleTempSpills(cur, prev *collector.Snapshot, _ *config.Config, _ *RuleExtras) []Finding {
	if !hasTempSpillWindow(cur, prev) {
		return nil
	}
	baseline := make(map[int64]collector.QueryStats, len(prev.Queries))
	for _, q := range prev.Queries {
		baseline[q.QueryID] = q
	}
	var out []Finding
	for _, q := range adviceQueries(cur) {
		old, ok := baseline[q.QueryID]
		if !ok || q.Calls <= old.Calls || q.TempBlksWritten < old.TempBlksWritten || q.TempBlksRead < old.TempBlksRead {
			continue
		}
		written := q.TempBlksWritten - old.TempBlksWritten
		if written < 128 {
			continue
		}
		out = append(out, Finding{
			Category: "temp_spill", Severity: "warning", ObjectType: "query",
			ObjectIdentifier: fmt.Sprintf("queryid:%d", q.QueryID),
			Title:            fmt.Sprintf("Query wrote %d temporary blocks in the collection interval", written),
			Detail: map[string]any{
				"queryid": fmt.Sprintf("%d", q.QueryID), "query": q.Query,
				"delta_temp_blks_written": written,
				"delta_temp_blks_read":    q.TempBlksRead - old.TempBlksRead,
				"delta_calls":             q.Calls - old.Calls,
				"interval_seconds":        cur.CollectedAt.Sub(prev.CollectedAt).Seconds(),
				"evidence_source":         "pg_stat_statements interval counters",
				"active_hint_validated":   false,
			},
			Recommendation: "Inspect EXPLAIN (ANALYZE, BUFFERS) for this query in a controlled session to identify a disk sort, hash spill, or another temporary-file operation. " +
				"Test a bounded SET LOCAL work_mem inside a transaction and compare the plan, latency, and temporary blocks; an index or query rewrite may avoid the spill. " +
				"Budget memory per sort/hash operation, parallel worker, and concurrent session before choosing a value. " +
				"On Lakebase, user-context work_mem can be set at session, role, or database scope; consider role/database scope only after validating the affected workload and restoring the previous setting. " +
				"No active hint or work_mem value has been validated; this recommendation does not require pg_hint_plan and does not change settings automatically.",
			ActionRisk: "moderate",
		})
	}
	return out
}
