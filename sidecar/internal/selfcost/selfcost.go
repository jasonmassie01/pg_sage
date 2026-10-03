// Package selfcost measures what pg_sage itself costs the database it
// monitors (perf v1.8.3): database time of its own statements, the blocks
// they touch, rows read and written in the sage schema, and the schema's
// size. pg_sage no longer hides from pg_stat_statements; every statement
// it sends carries the /* pg_sage */ tag, which is how its own entries are
// found.
package selfcost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Reading is one sample of cumulative counters.
type Reading struct {
	At       time.Time
	Database string
	// StatementsKnown is false when pg_stat_statements is not installed or
	// not preloaded: DBTimeMs, Calls and Blocks are then zero, not measured.
	StatementsKnown bool
	DBTimeMs        float64 // execution + planning time of pg_sage statements
	Calls           int64
	Blocks          int64 // shared blocks hit + read
	RowsRead        int64 // sage tables: sequential + index tuple reads
	RowsWritten     int64 // sage tables: rows inserted + updated + deleted
	SchemaBytes     int64 // sage relations with TOAST and indexes
	// Statements are pg_sage's pg_stat_statements entries; DBTimeMs, Calls
	// and Blocks are their sums.
	Statements map[StatementKey]StatementCounters
}

// StatementKey identifies a pg_stat_statements entry of this database.
type StatementKey struct {
	UserID   uint32
	QueryID  int64
	TopLevel bool
}

// StatementCounters are one entry's cumulative counters.
type StatementCounters struct {
	TimeMs float64 // execution + planning
	Calls  int64
	Blocks int64 // shared blocks hit + read
}

// Cost is pg_sage's cost per collector cycle between two readings.
type Cost struct {
	// Known is false without a usable previous reading (first sample,
	// statistics reset, no time elapsed): rates are then zero, not
	// measured. SchemaBytes is always the latest reading's.
	Known bool
	// DBTimeKnown additionally requires pg_stat_statements on both sides.
	DBTimeKnown         bool
	At                  time.Time
	Database            string
	WindowSeconds       float64
	CycleSeconds        float64
	DBTimeMsPerCycle    float64
	CallsPerCycle       float64
	BlocksPerCycle      float64
	RowsReadPerCycle    float64
	RowsWrittenPerCycle float64
	SchemaBytes         int64
}

// Querier is a pgx pool, connection or transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// statementsSQL sums this database's pg_sage-tagged statements.
const statementsSQL = `/* pg_sage */ SELECT userid, queryid, toplevel,
  (total_exec_time + total_plan_time)::float8, calls,
  (shared_blks_hit + shared_blks_read)::int8
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL AND strpos(query, '/* pg_sage') > 0`

// schemaSQL sums the sage relations' statistics counters and sizes. It
// reads pg_class once per measurement (one analyzer cycle).
const schemaSQL = `/* pg_sage */ SELECT current_database(),
  COALESCE(sum(pg_stat_get_tuples_returned(c.oid) + pg_stat_get_tuples_fetched(c.oid)
    + COALESCE((SELECT sum(pg_stat_get_tuples_fetched(i.indexrelid))
                  FROM pg_index i WHERE i.indrelid = c.oid), 0)), 0)::int8,
  COALESCE(sum(pg_stat_get_tuples_inserted(c.oid) + pg_stat_get_tuples_updated(c.oid)
    + pg_stat_get_tuples_deleted(c.oid)), 0)::int8,
  COALESCE(sum(pg_total_relation_size(c.oid)), 0)::int8
FROM pg_class c
WHERE c.relnamespace = to_regnamespace('sage') AND c.relkind IN ('r', 'm')`

// Read samples pg_sage's cumulative counters. A missing pg_stat_statements
// leaves the statement figures unknown; any other failure is an error.
func Read(ctx context.Context, q Querier) (Reading, error) {
	var r Reading
	err := q.QueryRow(ctx, schemaSQL).Scan(&r.Database, &r.RowsRead, &r.RowsWritten,
		&r.SchemaBytes)
	if err != nil {
		return Reading{}, fmt.Errorf("read sage schema counters: %w", err)
	}
	r.Statements, err = readStatements(ctx, q)
	switch {
	case err == nil:
		r.StatementsKnown = true
	case !statementsUnavailable(err):
		return Reading{}, fmt.Errorf("read pg_sage statements: %w", err)
	}
	for _, s := range r.Statements {
		r.DBTimeMs += s.TimeMs
		r.Calls += s.Calls
		r.Blocks += s.Blocks
	}
	r.At = time.Now()
	return r, nil
}

func readStatements(ctx context.Context, q Querier) (map[StatementKey]StatementCounters,
	error) {
	rows, err := q.Query(ctx, statementsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[StatementKey]StatementCounters{}
	for rows.Next() {
		var k StatementKey
		var s StatementCounters
		if err := rows.Scan(&k.UserID, &k.QueryID, &k.TopLevel, &s.TimeMs, &s.Calls,
			&s.Blocks); err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, rows.Err()
}

// statementDelta is pg_sage's work between two readings, entry by entry:
// an entry whose calls went back was reset and counts from zero, one that
// appeared counts in full, one evicted from pg_stat_statements drops out.
func statementDelta(prev, cur map[StatementKey]StatementCounters) StatementCounters {
	var d StatementCounters
	for k, c := range cur {
		p, ok := prev[k]
		if !ok || c.Calls < p.Calls || c.TimeMs < p.TimeMs || c.Blocks < p.Blocks {
			p = StatementCounters{}
		}
		d.TimeMs += c.TimeMs - p.TimeMs
		d.Calls += c.Calls - p.Calls
		d.Blocks += c.Blocks - p.Blocks
	}
	return d
}

// statementsUnavailable reports pg_stat_statements missing (not installed
// or not preloaded), as opposed to a failure worth surfacing.
func statementsUnavailable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P01", "55000", "42883":
		return true
	}
	return false
}

// Between is the cost per collector cycle (cycle) over the window between
// two readings.
func Between(prev, cur Reading, cycle time.Duration) Cost {
	c := Cost{At: cur.At, Database: cur.Database, SchemaBytes: cur.SchemaBytes,
		CycleSeconds: cycle.Seconds()}
	window := cur.At.Sub(prev.At).Seconds()
	if prev.At.IsZero() || window <= 0 || cycle <= 0 {
		return c
	}
	read, written := cur.RowsRead-prev.RowsRead, cur.RowsWritten-prev.RowsWritten
	if read < 0 || written < 0 {
		return c // statistics reset in the window
	}
	scale := cycle.Seconds() / window
	c.Known, c.WindowSeconds = true, window
	c.RowsReadPerCycle = float64(read) * scale
	c.RowsWrittenPerCycle = float64(written) * scale
	if !prev.StatementsKnown || !cur.StatementsKnown {
		return c
	}
	d := statementDelta(prev.Statements, cur.Statements)
	c.DBTimeKnown = true
	c.DBTimeMsPerCycle = d.TimeMs * scale
	c.CallsPerCycle = float64(d.Calls) * scale
	c.BlocksPerCycle = float64(d.Blocks) * scale
	return c
}

// OverBudget reports a known DB time per cycle above budgetMs; a budget
// of 0 or less disables the check.
func OverBudget(c Cost, budgetMs int) bool {
	return budgetMs > 0 && c.Known && c.DBTimeKnown && c.DBTimeMsPerCycle > float64(budgetMs)
}

// Meter keeps the previous reading and the latest cost. Safe for
// concurrent use (the analyzer observes, /metrics reads).
type Meter struct {
	mu   sync.Mutex
	prev Reading
	last Cost
}

// NewMeter returns an empty meter.
func NewMeter() *Meter { return &Meter{} }

// Observe records r and returns the cost since the previous reading.
func (m *Meter) Observe(r Reading, cycle time.Duration) Cost {
	if m == nil {
		return Cost{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := Between(m.prev, r, cycle)
	m.prev, m.last = r, c
	return c
}

// Last is the latest cost (zero before the first observation).
func (m *Meter) Last() Cost {
	if m == nil {
		return Cost{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}
