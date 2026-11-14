package cost

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-04b: the attribution window over real pg_stat_statements runs on
// the exclusive server and says when its premise (no entry was evicted
// between the samples) does not hold, instead of reporting "0 deltas".
//
// Invalid input, nil/empty and state transitions of QueryDeltas itself are
// covered by TestQueryDeltas_ResetDropsThePair and
// TestQueryDeltas_EmptyStoreIsEmpty; not repeated here.
// No concurrent-access tests: one workload connection, and testdb.Exclusive
// keeps every other writer off the server.

const premiseBroken = "the premise does not hold"

// statementWindow is one attribution window over real pg_stat_statements.
type statementWindow struct {
	deltas  []QueryDelta
	dropped int
}

// requireExclusiveDB opens a bootstrapped pool on a fresh database of the
// exclusive server, which t holds alone until it ends.
func requireExclusiveDB(t *testing.T, reason string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.Exclusive(t, reason)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the exclusive test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap the exclusive test database: %v", err)
	}
	return pool, ctx
}

// requireExclusiveFill is requireExclusiveDB for a test that fills
// pg_stat_statements: when it ends every entry is cleared (allowed only on
// the exclusive server), so the next holder starts with an empty table.
func requireExclusiveFill(t *testing.T, reason string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := requireExclusiveDB(t, reason)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
			t.Errorf("clear pg_stat_statements after filling it: %v", err)
		}
	})
	return pool, ctx
}

// statementPremise says whether pg_stat_statements kept every entry between
// two reads of pg_stat_statements_info.dealloc (the number of times it
// deallocated its least-used entries to make room).
func statementPremise(before, after int64) error {
	switch {
	case after > before:
		return fmt.Errorf("pg_stat_statements deallocated entries %d time(s) during the "+
			"window (dealloc %d -> %d), so the workload's fresh entries may be gone; %s",
			after-before, before, after, premiseBroken)
	case after < before:
		return fmt.Errorf("pg_stat_statements_info was reset during the window "+
			"(dealloc %d -> %d); %s", before, after, premiseBroken)
	}
	return nil
}

func readDealloc(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var dealloc int64
	if err := pool.QueryRow(ctx, `SELECT dealloc FROM pg_stat_statements_info`).
		Scan(&dealloc); err != nil {
		t.Fatalf("read pg_stat_statements_info.dealloc: %v", err)
	}
	return dealloc
}

// dbClock reads the server's clock. query_store.captured_at comes from it,
// and a container's clock can be more than a second away from the host's,
// so a window bounded by the host clock can miss both samples.
func dbClock(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the server clock: %v", err)
	}
	return now
}

// measureWindow clears this database's statements, records a sample before
// and after a workload (calling during, when set, between them) and returns
// the deltas, or the premise error when entries were evicted in the window.
// The window's bounds come from the server clock (dbClock).
func measureWindow(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	during func()) (statementWindow, error) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_store`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	from := dbClock(t, ctx, pool).Add(-time.Second)
	before, err := testdb.ResetStatements(ctx, pool)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	runWorkload(t, ctx, pool, 5)
	rec := querystore.NewRecorder()
	if _, err := rec.Record(ctx, pool, statementSamples(t, ctx, pool), time.Now()); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if during != nil {
		during()
	}
	runWorkload(t, ctx, pool, 40)
	if _, err := rec.Record(ctx, pool, statementSamples(t, ctx, pool), time.Now()); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	if err := statementPremise(before, readDealloc(t, ctx, pool)); err != nil {
		return statementWindow{}, err
	}
	to := dbClock(t, ctx, pool).Add(time.Second)
	deltas, dropped, err := QueryDeltas(ctx, pool, "orders", from, to)
	if err != nil {
		t.Fatalf("QueryDeltas: %v", err)
	}
	return statementWindow{deltas: deltas, dropped: dropped}, nil
}

// statementRoom returns pg_stat_statements.max and the entries held now,
// across every database of the server.
func statementRoom(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (limit, used int) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT current_setting('pg_stat_statements.max')::int,
		(SELECT count(*)::int FROM pg_stat_statements)`).Scan(&limit, &used)
	if err != nil {
		t.Fatalf("read pg_stat_statements capacity: %v", err)
	}
	return limit, used
}

// fillStatementsSQL runs n structurally distinct statements (a target list
// of a columns, the first a sum of b ones), so each gets its own queryid.
// They run nested, which pg_stat_statements tracks only with track = all.
const fillStatementsSQL = `DO $fill$
DECLARE
	n int := current_setting('pgsage.fill_n')::int;
	k int := ceil(sqrt(n))::int;
	done int := 0;
BEGIN
	FOR a IN 1..k LOOP
		FOR b IN 1..k LOOP
			EXIT WHEN done >= n;
			EXECUTE 'SELECT ' || repeat('1+', b) || '1' || repeat(', 1', a - 1);
			done := done + 1;
		END LOOP;
	END LOOP;
END
$fill$`

// fillStatements adds n new pg_stat_statements entries in one round trip.
func fillStatements(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	if n <= 0 {
		return
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("fill: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	steps := []struct {
		sql  string
		args []any
	}{
		{`SET LOCAL pg_stat_statements.track = 'all'`, nil},
		{`SELECT set_config('pgsage.fill_n', $1, true)`, []any{fmt.Sprint(n)}},
		{fillStatementsSQL, nil},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("fill pg_stat_statements with %d statements: %v", n, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("fill: commit: %v", err)
	}
}

func TestStatementPremise(t *testing.T) {
	if err := statementPremise(7, 7); err != nil {
		t.Fatalf("dealloc unchanged: %v, want the premise to hold", err)
	}
	if err := statementPremise(0, 0); err != nil {
		t.Fatalf("dealloc zero on both reads: %v, want the premise to hold", err)
	}
	err := statementPremise(2, 5)
	if err == nil || !strings.Contains(err.Error(), "deallocated entries 3 time(s)") ||
		!strings.Contains(err.Error(), "dealloc 2 -> 5") ||
		!strings.Contains(err.Error(), premiseBroken) {
		t.Fatalf("dealloc grew by 3: %v, want the eviction premise message", err)
	}
	err = statementPremise(4, 0)
	if err == nil || !strings.Contains(err.Error(), "was reset during the window") ||
		!strings.Contains(err.Error(), premiseBroken) {
		t.Fatalf("dealloc fell (full reset): %v, want the reset premise message", err)
	}
}

// Error propagation: filling pg_stat_statements past its maximum between the
// samples gives the premise message, not a bare delta count.
// (pg_stat_statements.max is restart-only, so the table is filled instead.)
func TestAttribution_EvictionInTheWindowBreaksThePremise(t *testing.T) {
	pool, ctx := requireExclusiveFill(t, "pg_stat_statements capacity")
	w, err := measureWindow(t, ctx, pool, func() {
		limit, used := statementRoom(t, ctx, pool)
		fillStatements(t, ctx, pool, limit-used+1000)
	})
	if err == nil {
		t.Fatalf("window with eviction returned %d deltas and no error, want %q",
			len(w.deltas), premiseBroken)
	}
	msg := err.Error()
	if !strings.Contains(msg, "deallocated entries") || !strings.Contains(msg, premiseBroken) ||
		strings.Contains(msg, "deltas") {
		t.Fatalf("eviction error = %q, want the dealloc premise message", msg)
	}
}

// Boundary: a table filled to within a few hundred entries of its maximum
// but never past it keeps dealloc unchanged, so the premise holds and the
// workload's deltas are all there.
func TestAttribution_NearlyFullTableKeepsThePremise(t *testing.T) {
	pool, ctx := requireExclusiveFill(t, "pg_stat_statements capacity")
	const margin = 500
	var limit, filled int
	w, err := measureWindow(t, ctx, pool, func() {
		var used int
		limit, used = statementRoom(t, ctx, pool)
		fillStatements(t, ctx, pool, limit-used-margin)
		_, filled = statementRoom(t, ctx, pool)
	})
	if err != nil {
		t.Fatalf("nearly full table (%d of %d entries): %v", filled, limit, err)
	}
	if filled < limit-margin-50 || filled > limit {
		t.Fatalf("filled to %d of %d entries, want within %d of the maximum",
			filled, limit, margin+50)
	}
	if len(w.deltas) < 2 || w.dropped != 0 {
		t.Fatalf("%d deltas (%d dropped) on a nearly full table, want the workload's queries",
			len(w.deltas), w.dropped)
	}
}
