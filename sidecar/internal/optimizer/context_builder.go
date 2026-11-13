package optimizer

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/workload"
)

// groupQueriesByTable maps each snapshot table to the application
// statements that name it, resolved like the tuning agent resolves them
// (workload.ReferencedTables): an unqualified name belongs to the only
// schema holding it, or to public when public is one of several.
func groupQueriesByTable(
	snap *collector.Snapshot,
) map[workload.Relation][]QueryInfo {
	result := make(map[workload.Relation][]QueryInfo)
	known := workload.KnownTables(snap)
	for _, q := range applicationQueries(snap.Queries) {
		qi := QueryInfo{
			QueryID:     q.QueryID,
			Text:        q.Query,
			Calls:       q.Calls,
			MeanTimeMs:  q.MeanExecTime,
			TotalTimeMs: q.TotalExecTime,
		}
		for _, t := range workload.ReferencedTables(q.Query, known) {
			result[t] = append(result[t], qi)
		}
	}
	return result
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

// writeRateKnown reports whether the table recorded any scan or write
// activity, i.e. whether WriteRate is evidence rather than a default
// zero (G3-B23).
func writeRateKnown(ts collector.TableStats) bool {
	return ts.SeqScan+ts.IdxScan+ts.NTupIns+ts.NTupUpd+ts.NTupDel > 0
}

// computeWriteRate is the percentage of index-maintaining writes among
// those writes plus the tuples read. Inserts and non-HOT updates add index
// entries; HOT updates and deletes do not (vacuum removes dead entries).
// Reads are tuples, not scans: a bulk COPY is one row per tuple written and
// must be weighed against the tuples lookups return, not their count.
func computeWriteRate(ts collector.TableStats) float64 {
	writes := float64(ts.NTupIns + max(ts.NTupUpd-ts.NTupHotUpd, 0))
	total := writes + float64(ts.SeqTupRead+ts.IdxTupFetch)
	if total == 0 {
		return 0
	}
	return writes / total * 100
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

// countIndexes counts the table's valid indexes: an invalid one (a failed
// CONCURRENTLY build) is the invalid-index rule's to drop, and does not
// hold a slot under max_indexes_per_table.
func countIndexes(
	indexes []collector.IndexStats,
	schema, table string,
) int {
	count := 0
	for _, idx := range indexes {
		if idx.SchemaName == schema && idx.RelName == table && idx.IsValid {
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
				OID:        idx.IndexRelID,
			})
		}
	}
	return result
}

// fetchColumns reads the table's columns. A failed read returns the
// error and no columns, never a partial list.
func fetchColumns(
	ctx context.Context,
	pool catalogread.Querier,
	schema, table string,
) ([]ColumnInfo, error) {
	// data_type is USER-DEFINED for every extension type (vector,
	// halfvec, geometry, ...); udt_name names the type itself.
	rows, err := pool.Query(ctx,
		`/* pg_sage optimizer */ SELECT column_name,
		   CASE WHEN data_type = 'USER-DEFINED' THEN udt_name ELSE data_type END,
		   is_nullable
		 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = $2
		 ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		var nullable string
		if err := rows.Scan(&c.Name, &c.Type, &nullable); err != nil {
			return nil, fmt.Errorf("scan a column of %s.%s: %w", schema, table, err)
		}
		c.IsNullable = nullable == "YES"
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %s.%s: %w", schema, table, err)
	}
	return cols, nil
}

func fetchColStats(
	ctx context.Context,
	pool catalogread.Querier,
	schema, table string,
	queries []QueryInfo,
) []ColStat {
	// Extract column names referenced in query texts to filter
	// pg_stats to only the columns the optimizer needs.
	cols := extractColumnsFromQueries(queries)
	if len(cols) > 0 {
		return fetchNamedColStats(ctx, pool, schema, table, cols)
	}
	// No column names extracted: return the most skewed
	// columns to cap prompt size.
	return scanColStats(pool.Query(ctx,
		`/* pg_sage optimizer */ SELECT attname, n_distinct, correlation,
			        most_common_vals::text, most_common_freqs::text
			 FROM pg_stats
			 WHERE schemaname = $1 AND tablename = $2
			 ORDER BY abs(correlation) DESC
			 LIMIT 15`, schema, table))
}

// fetchNamedColStats reads the pg_stats rows of the named columns.
func fetchNamedColStats(ctx context.Context, pool catalogread.Querier,
	schema, table string, cols []string) []ColStat {
	return scanColStats(pool.Query(ctx,
		`/* pg_sage optimizer */ SELECT attname, n_distinct, correlation,
			        most_common_vals::text, most_common_freqs::text
			 FROM pg_stats
			 WHERE schemaname = $1 AND tablename = $2
			   AND attname = ANY($3)`, schema, table, cols))
}

// scanColStats reads pg_stats rows; a failed read is no statistics (the
// rules that need them do not apply) and an unreadable row is skipped.
func scanColStats(rows pgx.Rows, err error) []ColStat {
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

// extractColumnsFromQueries extracts likely column names referenced
// in WHERE, ON, ORDER BY, and GROUP BY clauses from query texts.
// Uses simple keyword-based extraction; not a full SQL parser.
func extractColumnsFromQueries(queries []QueryInfo) []string {
	seen := make(map[string]bool)
	for _, q := range queries {
		cols := extractColumnRefs(q.Text)
		for _, c := range cols {
			seen[c] = true
		}
	}
	result := make([]string, 0, len(seen))
	for c := range seen {
		result = append(result, c)
	}
	return result
}

// extractColumnRefs extracts identifiers that follow WHERE, ON,
// ORDER BY, GROUP BY, AND, OR, SET keywords, or appear as the
// left side of comparison operators. Returns deduplicated names.
func extractColumnRefs(query string) []string {
	upper := strings.ToUpper(query)
	words := strings.Fields(upper)
	origWords := strings.Fields(query)

	triggers := map[string]bool{
		"WHERE": true, "AND": true, "OR": true, "ON": true,
		"SET": true, "BY": true, "HAVING": true,
	}

	var cols []string
	for i, w := range words {
		if !triggers[w] || i+1 >= len(origWords) {
			continue
		}
		candidate := origWords[i+1]
		col := cleanColumnRef(candidate)
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
}

// cleanColumnRef strips table aliases, quotes, and operators from
// a token to extract the bare column name. Returns "" if the token
// is not a plausible column reference.
func cleanColumnRef(token string) string {
	// Strip trailing operators and punctuation.
	token = strings.TrimRight(token, "=<>!(),;")
	if token == "" {
		return ""
	}
	// Handle table.column or alias.column references.
	if idx := strings.LastIndex(token, "."); idx >= 0 {
		token = token[idx+1:]
	}
	// Strip quoting.
	token = strings.Trim(token, "\"'`")
	// Reject SQL keywords, numbers, and placeholders.
	lower := strings.ToLower(token)
	rejects := map[string]bool{
		"select": true, "from": true, "join": true,
		"left": true, "right": true, "inner": true,
		"outer": true, "cross": true, "not": true,
		"null": true, "in": true, "is": true,
		"like": true, "between": true, "exists": true,
		"true": true, "false": true, "as": true,
		"case": true, "when": true, "then": true,
		"else": true, "end": true, "asc": true,
		"desc": true, "limit": true, "offset": true,
		"": true,
	}
	if rejects[lower] {
		return ""
	}
	// Reject if starts with $ (parameter placeholder).
	if strings.HasPrefix(lower, "$") {
		return ""
	}
	// Reject pure numbers.
	if _, err := strconv.Atoi(lower); err == nil {
		return ""
	}
	return lower
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
	tableQueries map[workload.Relation][]QueryInfo,
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
		child := workload.Relation{Schema: p.ChildSchema, Name: p.ChildTable}
		for _, q := range tableQueries[child] {
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

// SkipScanNote is the PostgreSQL 18 note for the index agent's table
// context, "" before 18.
func SkipScanNote(tc TableContext) string {
	if tc.PGMajor < skipScanMinMajor {
		return ""
	}
	return fmt.Sprintf("PostgreSQL 18 skip scan: a btree index can serve an equality "+
		"or range on its second key when its leading key has few distinct values (at "+
		"most %d), so a single-column index on a column an existing composite holds "+
		"second behind such a key is usually redundant.", skipScanMaxLeadingDistinct)
}
