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

// TableStat is one reading of a sage table's cumulative counters.
type TableStat struct {
	LiveRows   int64
	SeqScan    int64
	SeqTupRead int64
	IdxScan    int64
	Written    int64
}

// TableStats maps schema-qualified sage table names to their counters.
type TableStats map[string]TableStat

const tableStatsSQL = `/* ` + HarnessTag + ` */
SELECT s.schemaname || '.' || s.relname, GREATEST(s.n_live_tup, c.reltuples::bigint),
       COALESCE(s.seq_scan, 0), COALESCE(s.seq_tup_read, 0), COALESCE(s.idx_scan, 0),
       s.n_tup_ins + s.n_tup_upd + s.n_tup_del
FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid
WHERE s.schemaname = 'sage'`

// ReadTableStats reads the counters of every sage table.
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
		if err := rows.Scan(&name, &s.LiveRows, &s.SeqScan, &s.SeqTupRead, &s.IdxScan,
			&s.Written); err != nil {
			return nil, fmt.Errorf("perfgate: scan table statistics: %w", err)
		}
		out[name] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("perfgate: table statistics: %w", err)
	}
	return out, nil
}

// Delta is the change from before to these counters, one entry per table
// sorted by name; a table created in between counts from zero.
func (after TableStats) Delta(before TableStats) []TableDelta {
	out := make([]TableDelta, 0, len(after))
	for name, a := range after {
		b := before[name]
		out = append(out, TableDelta{
			Name: name, LiveRows: a.LiveRows,
			SeqScans: a.SeqScan - b.SeqScan, SeqTupRead: a.SeqTupRead - b.SeqTupRead,
			IdxScans: a.IdxScan - b.IdxScan, RowsWritten: a.Written - b.Written,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
