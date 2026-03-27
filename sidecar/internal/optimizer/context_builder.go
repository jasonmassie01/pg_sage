package optimizer

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
)

// BuildTableContexts creates enriched table contexts from a snapshot.
// Returns (contexts, planSource, error) where planSource indicates how
// execution plans were obtained ("explain_cache", "generic_plan",
// "query_text_only", or "none").
func BuildTableContexts(
	ctx context.Context,
	pool *pgxpool.Pool,
	snap *collector.Snapshot,
	planner *PlanCapture,
	minQueryCalls int64,
) ([]TableContext, string, error) {
	collation := fetchCollation(ctx, pool)
	tableQueries := groupQueriesByTable(snap)
	childSet := buildPartitionChildSet(snap)
	parentSet := buildPartitionParentSet(snap)

	planSource := "query_text_only"
	var contexts []TableContext
	for _, ts := range snap.Tables {
		if skipSchema(ts.SchemaName) {
			continue
		}
		key := ts.SchemaName + "." + ts.RelName

		// Skip partition children whose parent is in the snapshot.
		// PG11+ propagates indexes from parent to children.
		if childSet[key] {
			continue
		}

		queries := tableQueries[key]
		queries = filterByMinCalls(queries, minQueryCalls)
		if len(queries) == 0 {
			continue
		}

		isParent := parentSet[key]
		if isParent {
			queries = mergeChildQueries(
				queries, tableQueries, childSet, snap, key,
			)
		}

		tc := TableContext{
			Schema:         ts.SchemaName,
			Table:          ts.RelName,
			LiveTuples:     ts.NLiveTup,
			DeadTuples:     ts.NDeadTup,
			TableBytes:     ts.TableBytes,
			IndexBytes:     ts.IndexBytes,
			IndexCount:     countIndexes(
				snap.Indexes, ts.SchemaName, ts.RelName,
			),
			Queries:        queries,
			Collation:      collation,
			Relpersistence: ts.Relpersistence,
			IsPartitioned:  isParent,
		}
		tc.WriteRate = computeWriteRate(ts)
		tc.Workload = classifyWorkload(tc.WriteRate, tc.LiveTuples)
		tc.Columns = fetchColumns(ctx, pool, ts.SchemaName, ts.RelName)
		tc.Indexes = buildIndexInfo(snap.Indexes, ts.SchemaName, ts.RelName)
		tc.ColStats = fetchColStats(ctx, pool, ts.SchemaName, ts.RelName)

		if planner != nil {
			plans, source := planner.CapturePlans(ctx, snap.Queries)
			tc.Plans = filterPlansForTable(plans, queries)
			if len(plans) > 0 && planSource == "query_text_only" {
				planSource = source
			}
		}

		contexts = append(contexts, tc)
	}
	return contexts, planSource, nil
}

func fetchCollation(ctx context.Context, pool *pgxpool.Pool) string {
	var collation string
	err := pool.QueryRow(ctx, "SHOW lc_collate").Scan(&collation)
	if err != nil {
		return "C"
	}
	return collation
}

func groupQueriesByTable(
	snap *collector.Snapshot,
) map[string][]QueryInfo {
	result := make(map[string][]QueryInfo)
	for _, q := range snap.Queries {
		tables := extractTablesFromQuery(q.Query)
		qi := QueryInfo{
			QueryID:     q.QueryID,
			Text:        q.Query,
			Calls:       q.Calls,
			MeanTimeMs:  q.MeanExecTime,
			TotalTimeMs: q.TotalExecTime,
		}
		for _, t := range tables {
			result[t] = append(result[t], qi)
		}
	}
	return result
}

func extractTablesFromQuery(query string) []string {
	var tables []string
	upper := strings.ToUpper(query)
	words := strings.Fields(upper)
	for i, w := range words {
		if (w == "FROM" || w == "JOIN" || w == "UPDATE" || w == "INTO") &&
			i+1 < len(words) {
			table := strings.ToLower(words[i+1])
			table = strings.Trim(table, "\"(),;")
			if table == "" || strings.HasPrefix(table, "pg_") || table == "sage" {
				continue
			}
			if !strings.Contains(table, ".") {
				table = "public." + table
			}
			tables = append(tables, table)
		}
	}
	return tables
}

func filterByMinCalls(queries []QueryInfo, minCalls int64) []QueryInfo {
	var filtered []QueryInfo
	for _, q := range queries {
		if q.Calls >= minCalls {
			filtered = append(filtered, q)
		}
	}
	return filtered
}

func skipSchema(schema string) bool {
	skip := map[string]bool{
		"sage":               true,
		"pg_catalog":         true,
		"information_schema": true,
		"pg_toast":           true,
		"pg_temp":            true,
	}
	return skip[schema]
}

func computeWriteRate(ts collector.TableStats) float64 {
	total := float64(
		ts.SeqScan + ts.IdxScan + ts.NTupIns + ts.NTupUpd + ts.NTupDel,
	)
	if total == 0 {
		return 0
	}
	writes := float64(ts.NTupIns + ts.NTupUpd + ts.NTupDel)
	return (writes / total) * 100
}

func classifyWorkload(writeRatePct float64, liveTuples int64) string {
	if writeRatePct > 70 {
		return "oltp_write"
	}
	if writeRatePct < 10 && liveTuples > 100000 {
		return "olap"
	}
	if writeRatePct < 30 {
		return "oltp_read"
	}
	return "htap"
}

func countIndexes(
	indexes []collector.IndexStats,
	schema, table string,
) int {
	count := 0
	for _, idx := range indexes {
		if idx.SchemaName == schema && idx.RelName == table {
			count++
		}
	}
	return count
}

func buildIndexInfo(
	indexes []collector.IndexStats,
	schema, table string,
) []IndexInfo {
	var result []IndexInfo
	for _, idx := range indexes {
		if idx.SchemaName == schema && idx.RelName == table {
			result = append(result, IndexInfo{
				Name:       idx.IndexRelName,
				Definition: idx.IndexDef,
				Scans:      idx.IdxScan,
				IsUnique:   idx.IsUnique,
				IsValid:    idx.IsValid,
				SizeBytes:  idx.IndexBytes,
			})
		}
	}
	return result
}

func fetchColumns(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) []ColumnInfo {
	rows, err := pool.Query(ctx,
		`SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2
		 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		var nullable string
		if err := rows.Scan(&c.Name, &c.Type, &nullable); err != nil {
			continue
		}
		c.IsNullable = nullable == "YES"
		cols = append(cols, c)
	}
	return cols
}

func fetchColStats(
	ctx context.Context,
	pool *pgxpool.Pool,
	schema, table string,
) []ColStat {
	rows, err := pool.Query(ctx,
		`SELECT attname, n_distinct, correlation,
		       most_common_vals::text, most_common_freqs::text
		 FROM pg_stats
		 WHERE schemaname = $1 AND tablename = $2`, schema, table)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var stats []ColStat
	for rows.Next() {
		var s ColStat
		var corr *float64
		var mcvRaw, mcfRaw *string
		if err := rows.Scan(
			&s.Column, &s.NDistinct, &corr, &mcvRaw, &mcfRaw,
		); err != nil {
			continue
		}
		if corr != nil {
			s.Correlation = *corr
		}
		if mcvRaw != nil {
			s.MostCommonVals = parsePostgresArray(*mcvRaw)
		}
		if mcfRaw != nil {
			s.MostCommonFreqs = parseFloatArray(*mcfRaw)
		}
		stats = append(stats, s)
	}
	return stats
}

func filterPlansForTable(
	plans []PlanSummary,
	queries []QueryInfo,
) []PlanSummary {
	qids := make(map[int64]bool)
	for _, q := range queries {
		qids[q.QueryID] = true
	}
	var result []PlanSummary
	for _, p := range plans {
		if qids[p.QueryID] {
			result = append(result, p)
		}
	}
	return result
}

// buildPartitionChildSet returns a set of "schema.table" keys for
// tables that are partition children (have an inheritance parent).
func buildPartitionChildSet(
	snap *collector.Snapshot,
) map[string]bool {
	children := make(map[string]bool)
	for _, p := range snap.Partitions {
		key := p.ChildSchema + "." + p.ChildTable
		children[key] = true
	}
	return children
}

// buildPartitionParentSet returns a set of "schema.table" keys for
// tables that are partitioned parents (referenced in pg_inherits).
func buildPartitionParentSet(
	snap *collector.Snapshot,
) map[string]bool {
	parents := make(map[string]bool)
	for _, p := range snap.Partitions {
		key := p.ParentSchema + "." + p.ParentTable
		parents[key] = true
	}
	return parents
}

// mergeChildQueries folds queries targeting child partitions into
// the parent's query list, deduplicating by QueryID.
func mergeChildQueries(
	parentQueries []QueryInfo,
	tableQueries map[string][]QueryInfo,
	childSet map[string]bool,
	snap *collector.Snapshot,
	parentKey string,
) []QueryInfo {
	seen := make(map[int64]bool, len(parentQueries))
	for _, q := range parentQueries {
		seen[q.QueryID] = true
	}
	merged := append([]QueryInfo{}, parentQueries...)
	for _, p := range snap.Partitions {
		childKey := p.ChildSchema + "." + p.ChildTable
		pk := p.ParentSchema + "." + p.ParentTable
		if pk != parentKey || !childSet[childKey] {
			continue
		}
		for _, q := range tableQueries[childKey] {
			if !seen[q.QueryID] {
				seen[q.QueryID] = true
				merged = append(merged, q)
			}
		}
	}
	return merged
}

// parsePostgresArray parses a PostgreSQL text array representation like
// {val1,val2,val3} into a string slice.
func parsePostgresArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"")
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// parseFloatArray parses a PostgreSQL float array representation.
func parseFloatArray(s string) []float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	parts := strings.Split(s, ",")
	result := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if f, err := strconv.ParseFloat(p, 64); err == nil {
			result = append(result, f)
		}
	}
	return result
}
