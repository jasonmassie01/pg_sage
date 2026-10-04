package executor

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/verify"
)

// verifyTargetLimit bounds the queries an action is judged on (and the
// Bonferroni correction across them).
const verifyTargetLimit = 10

// tableStatementsSQL is the most-called statements naming a table ($1, a
// word-boundary regex); pg_stat_statements is shared memory, not a sage
// table, and is read once per action.
var tableStatementsSQL = `/* pg_sage */ SELECT s.queryid FROM pg_stat_statements s
	WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND s.query ~* $1 AND ` + selfmonitor.StatementExclusionSQL("s.query") + `
	GROUP BY s.queryid ORDER BY sum(s.calls) DESC LIMIT $2`

// workloadStatementsSQL is the statements a server setting is likely to
// move: those that spilled to temp files for work_mem ($1 true), the
// costliest otherwise.
var workloadStatementsSQL = `/* pg_sage */ SELECT s.queryid FROM pg_stat_statements s
	WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND (NOT $1 OR s.temp_blks_written > 0) AND ` +
	selfmonitor.StatementExclusionSQL("s.query") + `
	GROUP BY s.queryid
	ORDER BY CASE WHEN $1 THEN sum(s.temp_blks_written)::float8
	              ELSE sum(s.total_exec_time) END DESC
	LIMIT $2`

// planReferencedSQL is the targets whose latest captured plans name the
// index: one index range of idx_explain_queryid per target.
const planReferencedSQL = `/* pg_sage */ SELECT q.id FROM unnest($1::bigint[]) AS q(id)
	WHERE EXISTS (SELECT 1 FROM (SELECT e.plan_json FROM sage.explain_cache e
	                WHERE e.queryid = q.id ORDER BY e.captured_at DESC LIMIT 3) p
	              WHERE strpos(p.plan_json::text, $2) > 0)`

const droppedIndexTableSQL = `/* pg_sage */ SELECT c.relname, ic.relname
	FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
	JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indexrelid = to_regclass($1)`

// dropTargets selects the queries a drop could hurt: the most-called
// statements on the index's table. It records the dropped index and which
// targets' plans used it (evidence for the soft-drop).
func (e *Executor) dropTargets(ctx context.Context, sql string, before map[string]any) []int64 {
	index := firstObjectAfter(sql, "DROP INDEX", "CONCURRENTLY", "IF", "EXISTS")
	var table, bare string
	if err := e.pool.QueryRow(ctx, droppedIndexTableSQL, index).Scan(&table, &bare); err != nil {
		e.logFn("executor", "drop targets for %s unavailable (reads unproven): %v", index, err)
		return nil
	}
	before["dropped_index"] = bare
	ids := e.statementIDs(ctx, tableStatementsSQL, wordPattern(table), verifyTargetLimit)
	if len(ids) > 0 {
		before["plan_referenced_queryids"] = e.statementIDs(ctx, planReferencedSQL, ids, bare)
	}
	return ids
}

// configTargets selects the queries a setting change could hurt: for a
// table's storage parameters the statements on that table, for a server
// setting the statements it is likely to move.
func (e *Executor) configTargets(ctx context.Context, sql string, before map[string]any) []int64 {
	change, _ := before["config_change"].(map[string]any)
	if table, _ := change["table"].(string); table != "" {
		parts := splitQualifiedIdentifier(table)
		return e.statementIDs(ctx, tableStatementsSQL, wordPattern(parts[len(parts)-1]),
			verifyTargetLimit)
	}
	name, _ := change["name"].(string)
	if name == "" {
		name = configParamFromSQL(sql)
	}
	return e.statementIDs(ctx, workloadStatementsSQL, name == "work_mem", verifyTargetLimit)
}

func wordPattern(name string) string {
	return `\m` + regexp.QuoteMeta(unquoteIdentifier(name)) + `\M`
}

// statementIDs runs a queryid-returning query; a failure (for example no
// pg_stat_statements) is logged and yields no targets.
func (e *Executor) statementIDs(ctx context.Context, query string, args ...any) []int64 {
	rows, err := e.pool.Query(ctx, query, args...)
	if err != nil {
		e.logFn("executor", "select verification targets: %v", err)
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			e.logFn("executor", "scan verification target: %v", err)
			return nil
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		e.logFn("executor", "read verification targets: %v", err)
		return nil
	}
	return ids
}

// maintenanceBaseline records the VACUUM/ANALYZE metric's value now (dead
// tuples, rows modified since analyze) as the prediction's baseline.
func (e *Executor) maintenanceBaseline(
	ctx context.Context, sql string, p *verify.Prediction, before map[string]any,
) {
	table := vacuumObject(sql)
	if p.Class == verify.ClassAnalyze {
		table = firstObjectAfter(sql, "ANALYZE", "VERBOSE")
	}
	value, err := readMaintenanceMetric(ctx, e.pool, p.Metric, table)
	if err != nil {
		e.logFn("executor", "maintenance baseline for %s unavailable: %v", table, err)
		return
	}
	p.Baseline = &value
	before["maintenance_table"] = table
}

// freezeBaseline measures the targets before the action: the window is
// the class's minimum (the business cycle for a drop), doubled up to the
// cap until the targets have enough calls. It is frozen in before_state
// so a long window is never compared with a baseline retention removed.
func (e *Executor) freezeBaseline(
	ctx context.Context, class string, ids []int64,
) map[string]verify.Measurement {
	cfg, _, _ := e.policySnapshot()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	window, capW, minCalls := baselineWindows(class, cfg)
	source := verify.NewPostgresObservationSource(e.pool)
	now := time.Now()
	var got map[int64]verify.Measurement
	for {
		measured, err := source.QueryMeasurements(ctx, ids, now.Add(-window), now)
		if err != nil {
			e.logFn("executor", "freeze verification baseline: %v", err)
			return nil
		}
		got = measured
		if pooledCalls(got) >= minCalls || window >= capW {
			break
		}
		window = min(window*2, capW)
	}
	out := make(map[string]verify.Measurement, len(got))
	for id, m := range got {
		out[strconv.FormatInt(id, 10)] = m
	}
	return out
}

func baselineWindows(class string, cfg *config.Config) (time.Duration, time.Duration, int) {
	minCalls := cfg.Verify.MinSamples
	if minCalls <= 0 {
		minCalls = verify.DefaultMinSamples
	}
	if class == verify.ClassIndexDrop {
		w := cfg.Verify.DropWindow()
		if w <= 0 {
			w = time.Duration(config.DefaultVerifyDropWindowHours) * time.Hour
		}
		return w, w, minCalls
	}
	window := time.Duration(cfg.Verify.WindowMinutes) * time.Minute
	if window <= 0 {
		window = 2 * time.Hour
	}
	capW := max(time.Duration(cfg.Verify.WindowMaxMinutes)*time.Minute, window)
	return window, capW, minCalls
}

func pooledCalls(ms map[int64]verify.Measurement) int {
	total := 0
	for _, m := range ms {
		total += m.Samples
	}
	return total
}

// bareIndexName is the unqualified, unquoted name of "schema.index".
func bareIndexName(qualified string) string {
	parts := splitQualifiedIdentifier(strings.TrimSpace(qualified))
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
