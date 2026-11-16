package tuning

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The rehearsal measurer for callers outside tuning (lab-04b: the
// migration rehearsal measures the migrated table's statements with it).
// The rules are the index rehearsal's: EXPLAIN (ANALYZE, BUFFERS) only
// for a statement with no $n parameters that explain.ProveRead proves
// read-only, a plan otherwise, always in a READ ONLY rolled-back
// transaction on the application search_path under the timeout.

// Errors a clone measurement returns.
var (
	// ErrStatementTimedOut: an executed statement hit the timeout.
	ErrStatementTimedOut = errAnalyzeTimedOut
	// ErrNotOneStatement: the text holds more than one statement.
	ErrNotOneStatement = errMultiStatement
)

// CloneMeasurement is one statement measured on a clone. Blocks is nil
// and NotExecuted says why when the statement was planned only; LatencyMS
// is the execution time of an executed statement; Plan is the EXPLAIN
// (FORMAT JSON) document.
type CloneMeasurement struct {
	Cost        float64
	Blocks      *int64
	LatencyMS   float64
	Plan        []byte
	NotExecuted string
}

// Executed reports whether the statement ran (EXPLAIN ANALYZE).
func (m CloneMeasurement) Executed() bool { return m.Blocks != nil }

// CloneMeasurer measures statements on one clone connection.
type CloneMeasurer struct {
	m measurer
}

// NewCloneMeasurer measures on conn, a connection to a clone already shown
// not to be production. timeout bounds each executed statement (0:
// DefaultAnalyzeTimeout); inspect is the analyze guard's parser (nil:
// libpg_query, absent without cgo, which then executes nothing).
func NewCloneMeasurer(ctx context.Context, conn *pgx.Conn, timeout time.Duration,
	inspect func(string) (sqlast.ReadQuery, error)) (CloneMeasurer, error) {
	if conn == nil {
		return CloneMeasurer{}, fmt.Errorf("measure on the clone: no connection")
	}
	if timeout <= 0 {
		timeout = DefaultAnalyzeTimeout
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		return CloneMeasurer{}, fmt.Errorf("clone version: %w", err)
	}
	return CloneMeasurer{m: measurer{conn: conn, version: version,
		opts: RehearsalOptions{AnalyzeTimeout: timeout, Inspect: inspect}}}, nil
}

// Measure executes or plans one statement; see the file comment.
func (c CloneMeasurer) Measure(ctx context.Context, text string) (CloneMeasurement, error) {
	got, err := c.m.measure(ctx, text)
	out := CloneMeasurement{Cost: got.cost, Blocks: got.blocks, LatencyMS: got.latencyMS,
		Plan: got.plan, NotExecuted: got.notExecuted}
	return out, err
}
