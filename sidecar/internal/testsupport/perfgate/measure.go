package perfgate

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Statement tags. The gate's own statements and the synthetic workload's
// carry one, and ReadStatements leaves them out: what remains is pg_sage.
const (
	HarnessTag  = "perfgate:harness"
	WorkloadTag = "perfgate:workload"
)

// ErrStatementsUnavailable reports that pg_stat_statements is not loaded
// through shared_preload_libraries.
var ErrStatementsUnavailable = errors.New("perfgate: pg_stat_statements is not preloaded")

// Querier is a pgx pool, connection or transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Prepare installs pg_stat_statements in the database and checks that it
// is collecting.
func Prepare(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return fmt.Errorf("perfgate: create pg_stat_statements: %w", err)
	}
	var preload string
	if err := q.QueryRow(ctx, "/* "+HarnessTag+" */ "+
		"SELECT current_setting('shared_preload_libraries')").Scan(&preload); err != nil {
		return fmt.Errorf("perfgate: read shared_preload_libraries: %w", err)
	}
	var n int64
	err := q.QueryRow(ctx, "/* "+HarnessTag+" */ SELECT count(*) FROM pg_stat_statements").Scan(&n)
	if err != nil {
		return fmt.Errorf("%w (shared_preload_libraries=%q): %v", ErrStatementsUnavailable,
			preload, err)
	}
	return nil
}

// ResetStatements clears pg_stat_statements.
func ResetStatements(ctx context.Context, q Querier) error {
	_, err := q.Exec(ctx, "/* "+HarnessTag+" */ SELECT pg_stat_statements_reset()")
	if err != nil {
		return fmt.Errorf("perfgate: reset pg_stat_statements: %w", err)
	}
	return nil
}

const statementsSQL = `/* ` + HarnessTag + ` */
SELECT s.queryid, s.query, s.calls, s.total_exec_time, s.mean_exec_time,
       s.max_exec_time, s.rows
FROM pg_stat_statements s
JOIN pg_database d ON d.oid = s.dbid
WHERE d.datname = $1 AND s.calls > 0
  AND strpos(s.query, $2) = 0 AND strpos(s.query, $3) = 0
ORDER BY s.total_exec_time DESC`

// ReadStatements returns the database's statements other than the
// harness's and the workload's, costliest first.
func ReadStatements(ctx context.Context, q Querier, database string) ([]Statement, error) {
	rows, err := q.Query(ctx, statementsSQL, database, HarnessTag, WorkloadTag)
	if err != nil {
		return nil, fmt.Errorf("perfgate: read pg_stat_statements: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Statement, error) {
		var s Statement
		err := r.Scan(&s.QueryID, &s.Query, &s.Calls, &s.TotalMs, &s.MeanMs, &s.MaxMs, &s.Rows)
		return s, err
	})
}

// TableStat is one reading of a sage relation's cumulative counters. A
// partition carries its partitioned table's name in Parent: phases report
// the logical table (sage.query_store), not each day's partition.
type TableStat struct {
	Parent     string // schema-qualified partitioned table; "" for a plain table
	LiveRows   int64
	SeqScan    int64
	SeqTupRead int64
	IdxScan    int64
	Written    int64
	Updated    int64
	HotUpdated int64
	Bytes      int64 // heap, TOAST and indexes
}

// TableStats maps schema-qualified sage relation names to their counters.
type TableStats map[string]TableStat

const tableStatsSQL = `/* ` + HarnessTag + ` */
SELECT s.schemaname || '.' || s.relname,
       COALESCE((SELECT pn.nspname || '.' || pc.relname FROM pg_inherits i
                 JOIN pg_class pc ON pc.oid = i.inhparent
                 JOIN pg_namespace pn ON pn.oid = pc.relnamespace
                 WHERE i.inhrelid = s.relid), ''),
       GREATEST(s.n_live_tup, c.reltuples::bigint),
       COALESCE(s.seq_scan, 0), COALESCE(s.seq_tup_read, 0), COALESCE(s.idx_scan, 0),
       s.n_tup_ins + s.n_tup_upd + s.n_tup_del, s.n_tup_upd, s.n_tup_hot_upd,
       pg_total_relation_size(s.relid)
FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid
WHERE s.schemaname = 'sage'`

// ReadTableStats reads the counters of every sage table and partition.
func ReadTableStats(ctx context.Context, q Querier) (TableStats, error) {
	rows, err := q.Query(ctx, tableStatsSQL)
	if err != nil {
		return nil, fmt.Errorf("perfgate: read table statistics: %w", err)
	}
	defer rows.Close()
	out := TableStats{}
	for rows.Next() {
		var name string
		var s TableStat
		if err := rows.Scan(&name, &s.Parent, &s.LiveRows, &s.SeqScan, &s.SeqTupRead,
			&s.IdxScan, &s.Written, &s.Updated, &s.HotUpdated, &s.Bytes); err != nil {
			return nil, fmt.Errorf("perfgate: scan table statistics: %w", err)
		}
		out[name] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("perfgate: table statistics: %w", err)
	}
	return out, nil
}

// logical is the table a relation's activity is charged to.
func (s TableStat) logical(name string) string {
	if s.Parent != "" {
		return s.Parent
	}
	return name
}

// Delta is the change from before to these counters, one entry per
// logical table sorted by name. Counters are differenced per relation and
// summed per partitioned table: a relation created in between counts from
// zero; a partition dropped in between adds nothing to the counters but
// lowers BytesGrowth (size at the end minus size at the start).
func (after TableStats) Delta(before TableStats) []TableDelta {
	sums := map[string]*TableDelta{}
	for name, a := range after {
		b := before[name]
		d := sums[a.logical(name)]
		if d == nil {
			d = &TableDelta{Name: a.logical(name)}
			sums[d.Name] = d
		}
		d.LiveRows += a.LiveRows
		d.SeqScans += a.SeqScan - b.SeqScan
		if a.SeqScan > b.SeqScan {
			d.ScannedRelationRows = max(d.ScannedRelationRows, a.LiveRows)
		}
		d.SeqTupRead += a.SeqTupRead - b.SeqTupRead
		d.IdxScans += a.IdxScan - b.IdxScan
		d.RowsWritten += a.Written - b.Written
		d.Updates += a.Updated - b.Updated
		d.HotUpdates += a.HotUpdated - b.HotUpdated
		d.Bytes += a.Bytes
		d.BytesGrowth += a.Bytes
		d.Relations++
	}
	for name, b := range before {
		if d := sums[b.logical(name)]; d != nil {
			d.BytesGrowth -= b.Bytes
		}
	}
	out := make([]TableDelta, 0, len(sums))
	for _, d := range sums {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
