package forecaster

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The cache findings carry the evidence the advice needs (forecast-04):
// the tables whose heap reads missed shared_buffers most over the judged
// day, and shared_buffers itself.

const (
	// cacheTopTables is how many tables the evidence names.
	cacheTopTables = 5
	// cacheEvidenceTimeout bounds both evidence reads.
	cacheEvidenceTimeout = 10 * time.Second
)

// cacheTopTablesSQL ranks the tables by heap_blks_read growth between the
// first and the last non-empty 'tables' snapshot of the day starting at
// $1: two snapshots are decoded, whatever the day's count. A table whose
// count fell (a statistics reset) or that is missing from either side
// (created that day, or a row written before heap_blks_read existed) is
// left out rather than guessed.
var cacheTopTablesSQL = `/* pg_sage */
WITH ids AS (
    SELECT (SELECT s.id FROM sage.snapshots s
             WHERE {db:s} AND s.category = 'tables' AND s.collected_at >= $1
               AND s.collected_at < $1 + interval '1 day'
               AND ` + snapstore.NonEmptySQL("s") + `
             ORDER BY s.collected_at, s.id LIMIT 1) AS lo,
           (SELECT s.id FROM sage.snapshots s
             WHERE {db:s} AND s.category = 'tables' AND s.collected_at >= $1
               AND s.collected_at < $1 + interval '1 day'
               AND ` + snapstore.NonEmptySQL("s") + `
             ORDER BY s.collected_at DESC, s.id DESC LIMIT 1) AS hi
), docs AS MATERIALIZED (
    SELECT (SELECT ` + snapstore.DataSQL("s") + ` FROM sage.snapshots s
             WHERE {db:s} AND s.id = ids.lo) AS lo,
           (SELECT ` + snapstore.DataSQL("s") + ` FROM sage.snapshots s
             WHERE {db:s} AND s.id = ids.hi) AS hi
      FROM ids
)
SELECT h.e->>'schemaname', h.e->>'relname',
       (h.e->>'heap_blks_read')::int8 - (l.e->>'heap_blks_read')::int8 AS reads
  FROM docs
 CROSS JOIN LATERAL jsonb_array_elements(docs.hi) AS h(e)
  JOIN LATERAL jsonb_array_elements(docs.lo) AS l(e)
    ON l.e->>'schemaname' = h.e->>'schemaname' AND l.e->>'relname' = h.e->>'relname'
 WHERE (h.e->>'heap_blks_read')::int8 > (l.e->>'heap_blks_read')::int8
 ORDER BY reads DESC, 1, 2
 LIMIT $2`

// tableHeapReads is one table's heap blocks read over the judged day.
type tableHeapReads struct {
	Schema, Table string
	Reads         int64
}

// queryCacheTopTables returns the limit tables with the most heap blocks
// read over the day starting at day, most first.
func queryCacheTopTables(ctx context.Context, pool *pgxpool.Pool, day time.Time,
	limit int) ([]tableHeapReads, error) {
	rows, err := histstore.Resolve(pool).Query(ctx, cacheTopTablesSQL, day, limit)
	if err != nil {
		return nil, fmt.Errorf("query cache top tables of %s: %w", day.Format(time.RFC3339), err)
	}
	defer rows.Close()
	var out []tableHeapReads
	for rows.Next() {
		var t tableHeapReads
		if err := rows.Scan(&t.Schema, &t.Table, &t.Reads); err != nil {
			return nil, fmt.Errorf("scan cache top tables: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache top tables: %w", err)
	}
	return out, nil
}

// sharedBuffersSQL reads the setting with its unit (for example 128MB): a
// read-only call on the monitored database that touches no relation.
const sharedBuffersSQL = `/* pg_sage */ SELECT current_setting('shared_buffers')`

// addCacheEvidence reads the evidence once and attaches it to every
// cache finding. A failed read is logged and that part is left out: the
// finding stands on the ratio alone.
func (f *Forecaster) addCacheEvidence(ctx context.Context, findings []analyzer.Finding) {
	if len(findings) == 0 {
		return
	}
	day, ok := findings[0].Detail["day"].(time.Time)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cacheEvidenceTimeout)
	defer cancel()
	evidence := map[string]any{}
	if tables, err := queryCacheTopTables(ctx, f.pool, day, cacheTopTables); err != nil {
		f.logFn("WARN", "forecaster: cache evidence: %v", err)
	} else {
		evidence["top_tables_by_heap_blks_read"] = tableReadsDetail(tables)
	}
	var sharedBuffers string
	if err := f.pool.QueryRow(ctx, sharedBuffersSQL).Scan(&sharedBuffers); err != nil {
		f.logFn("WARN", "forecaster: cache evidence: read shared_buffers: %v", err)
	} else {
		evidence["shared_buffers"] = sharedBuffers
	}
	for i := range findings {
		for k, v := range evidence {
			findings[i].Detail[k] = v
		}
	}
}

// tableReadsDetail is the evidence's table list, JSON-shaped.
func tableReadsDetail(tables []tableHeapReads) []map[string]any {
	out := make([]map[string]any, len(tables))
	for i, t := range tables {
		out[i] = map[string]any{"table": t.Schema + "." + t.Table, "heap_blks_read": t.Reads}
	}
	return out
}
