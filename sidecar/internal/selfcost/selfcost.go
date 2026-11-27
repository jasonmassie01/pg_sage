// Package selfcost measures what pg_sage itself costs the database it
// monitors (perf v1.8.3): database time of its own statements, the blocks
// they touch, rows read and written in the sage schema, and the schema's
// size. pg_sage no longer hides from pg_stat_statements; every statement
// it sends carries the /* pg_sage */ tag, which is how its own entries are
// found, and the tag may name the component that sent it
// (/* pg_sage collector */), which is how the cost is attributed.
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
	// StatementsKnown is false when pg_stat_statements is not installed,
	// not preloaded, older than 1.8 or failed to read (StatementsErr):
	// DBTimeMs, Calls and Blocks are then zero, not measured.
	StatementsKnown bool
	StatementsErr   error
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
	Blocks int64  // shared blocks hit + read
	Text   string // the normalized statement, first 2000 characters
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

// Read samples pg_sage's cumulative counters once, without a key cache
// (every sample reads the tagged texts): one schema read and one sample of
// the statements. A failed schema read is an error; the statements' state
// is in the reading (Sample).
func Read(ctx context.Context, q Querier) (Reading, error) {
	sc, err := ReadSchema(ctx, q)
	if err != nil {
		return Reading{}, err
	}
	return NewSampler(nil).Sample(ctx, q, sc), nil
}

// Sample combines the cycle's schema reading with a sample of pg_sage's
// statements. A statement failure leaves the schema counters: the reading
// then has its statements unknown and the failure in StatementsErr; a
// missing or outdated pg_stat_statements is unknown with no error.
func (s *Sampler) Sample(ctx context.Context, q Querier, sc Schema) Reading {
	r := Reading{Database: sc.Database, RowsRead: sc.RowsRead,
		RowsWritten: sc.RowsWritten, SchemaBytes: sc.Bytes}
	stmts, known, err := s.Statements(ctx, q)
	if err != nil {
		r.StatementsErr = fmt.Errorf("read pg_sage statements: %w", err)
	}
	r.Statements, r.StatementsKnown = stmts, known
	for _, st := range r.Statements {
		r.DBTimeMs += st.TimeMs
		r.Calls += st.Calls
		r.Blocks += st.Blocks
	}
	r.At = time.Now()
	return r
}

// statementDelta is pg_sage's work between two readings, entry by entry:
// an entry whose calls went back was reset and counts from zero, one that
// appeared counts in full, one evicted from pg_stat_statements drops out.
func statementDelta(prev, cur map[StatementKey]StatementCounters) StatementCounters {
	var d StatementCounters
	for k, c := range cur {
		e := entryDelta(prev, k, c)
		d.TimeMs += e.TimeMs
		d.Calls += e.Calls
		d.Blocks += e.Blocks
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

// Meter keeps the previous reading and the latest cost, its costliest
// statements and its cost per component. Safe for concurrent use (the
// analyzer observes, /metrics and the API read).
type Meter struct {
	mu         sync.Mutex
	prev       Reading
	last       Cost
	top        []StatementCost
	components []ComponentCost
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
	m.top, m.components = nil, nil
	if c.DBTimeKnown {
		m.top = TopStatements(m.prev.Statements, r.Statements, TopN)
		m.components = ByComponent(m.prev.Statements, r.Statements, c.WindowSeconds)
	}
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
