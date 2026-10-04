package executor

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Dogfood round 2 (item 4): CREATE STATISTICS and REINDEX are verified by
// the post-action monitor (internal/verify statistics.go, reindex.go), so
// the trust ledger can earn them instead of only carrying them over. Both
// are watched even without a rollback statement: REINDEX has none, and a
// regression is then recorded, never "rolled back".

// estimateLookback is how far back the pre-action plans of a statistics
// action's targets are read for its estimate baseline.
const estimateLookback = 7 * 24 * time.Hour

// monitoredWithoutRollback reports a class the monitor watches even when
// the action has no rollback statement.
func monitoredWithoutRollback(sql string) bool {
	switch verificationClass(sql) {
	case verify.ClassStatistics, verify.ClassReindex:
		return true
	}
	return false
}

// statisticsTable is the table a CREATE STATISTICS reads: the name after
// its last top-level FROM (expressions in parentheses may hold their own
// FROM, e.g. extract(year from d)); "" when there is none.
func statisticsTable(sql string) string {
	text := normalizeSQLText(sql)
	if !strings.HasPrefix(strings.ToUpper(text), "CREATE STATISTICS") {
		return ""
	}
	depth, quoted, from := 0, false, -1
	upper := strings.ToUpper(text)
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(upper[i:], "FROM") &&
			(i == 0 || text[i-1] == ' ') && (i+4 == len(text) || text[i+4] == ' '):
			from = i + 4
		}
	}
	if from < 0 {
		return ""
	}
	fields := strings.Fields(text[from:])
	if len(fields) == 0 {
		return ""
	}
	return cleanupIdentifierToken(fields[0])
}

// reindexTarget is the index or table a REINDEX rebuilds and whether it
// is a table; "" for a schema, database or system REINDEX.
func reindexTarget(sql string) (string, bool) {
	text := normalizeSQLText(sql)
	fields := strings.Fields(text)
	i := skipOptionList(fields, 1)
	if i >= len(fields) {
		return "", false
	}
	kind := strings.ToUpper(fields[i])
	if kind != "INDEX" && kind != "TABLE" {
		return "", false
	}
	return reindexObject(text), kind == "TABLE"
}

// tableTargets are the most-called workload statements on a table.
func (e *Executor) tableTargets(ctx context.Context, table string) []int64 {
	parts := splitQualifiedIdentifier(table)
	if table == "" || len(parts) == 0 {
		return nil
	}
	return e.statementIDs(ctx, tableStatementsSQL, wordPattern(parts[len(parts)-1]),
		verifyTargetLimit)
}

// statisticsBaseline records the table and, for the targets (the
// finding's queries, else the statements on the table), the row-estimate
// error of their plans sampled before the action.
func (e *Executor) statisticsBaseline(
	ctx context.Context, sql string, p *verify.Prediction, before map[string]any,
) {
	table := statisticsTable(sql)
	before["statistics_table"] = table
	before["statistics_analyze_mark"] = e.analyzeMark(ctx, table)
	if len(p.TargetQueryIDs) == 0 {
		p.TargetQueryIDs = e.tableTargets(ctx, table)
	}
	if len(p.TargetQueryIDs) == 0 {
		return
	}
	now := time.Now()
	sample, err := verify.NewPostgresObservationSource(e.pool).EstimateErrors(ctx,
		p.TargetQueryIDs, now.Add(-estimateLookback), now)
	if err != nil {
		e.logFn("executor", "estimate baseline for %s unavailable: %v", table, err)
		return
	}
	before["estimate_baseline"] = sample
}

// reindexBaseline records the REINDEX target with its size now, and the
// statements on its table as the targets a rebuild could hurt.
func (e *Executor) reindexBaseline(
	ctx context.Context, sql string, p *verify.Prediction, before map[string]any,
) {
	target, isTable := reindexTarget(sql)
	if target == "" {
		return
	}
	bytes, _, err := verify.NewPostgresObservationSource(e.pool).IndexFootprint(ctx,
		target, isTable)
	if err != nil {
		e.logFn("executor", "reindex baseline for %s unavailable: %v", target, err)
		return
	}
	before["reindex_target"], before["reindex_table"] = target, isTable
	before["reindex_bytes"] = bytes
	table := target
	if !isTable {
		var name, bare string
		if err := e.pool.QueryRow(ctx, droppedIndexTableSQL, target).
			Scan(&name, &bare); err != nil {
			e.logFn("executor", "table of index %s unavailable: %v", target, err)
			return
		}
		table = name
	}
	if len(p.TargetQueryIDs) == 0 {
		p.TargetQueryIDs = e.tableTargets(ctx, table)
	}
}

// defaultReindexShrinkPct is the predicted size reduction of a REINDEX
// whose producer gave no bloat estimate (verify.IndexShrinkPct).
const defaultReindexShrinkPct = float64(verify.IndexShrinkPct)

// r2Prediction is the rule-based prediction of a statistics or reindex
// action: CREATE STATISTICS halves the targets' row-estimate error; a
// REINDEX reclaims the bloat its producer measured (bloat_pct, a share in
// (0, 100)), else at least defaultReindexShrinkPct.
func r2Prediction(class string, detail map[string]any) verify.Prediction {
	if class == verify.ClassStatistics {
		p := ruleBasedPrediction(class, verify.MetricRowEstimateError, -50,
			"CREATE STATISTICS is expected to halve the targeted queries' row-estimate error")
		p.TargetQueryIDs = targetQueryIDs(analyzer.Finding{Detail: detail})
		return p
	}
	shrink := defaultReindexShrinkPct
	for _, key := range []string{"bloat_pct", "estimated_bloat_pct", "index_bloat_pct"} {
		if v, ok := detailFloat(detail[key]); ok && v > 0 && v < 100 {
			shrink = v
			break
		}
	}
	return ruleBasedPrediction(class, verify.MetricIndexBytes, -shrink,
		"REINDEX is expected to reclaim the index's bloat")
}
