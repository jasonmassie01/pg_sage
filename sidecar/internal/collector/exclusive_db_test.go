package collector

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// exclusivePool gives t a fresh database on the exclusive test server,
// held for t alone (testdb.Exclusive), and a pool on it: for tests that
// force a CHECKPOINT or read cluster-wide counters (ci-13).
func exclusivePool(t *testing.T, reason string) *pgxpool.Pool {
	t.Helper()
	return connectPool(t, testdb.Exclusive(t, reason))
}

// exclusiveTextTestDB is textTestDB on a fresh database of the exclusive
// test server, held for t alone: for tests that rank or count every
// pg_stat_statements row, which every other package's statements (and
// resets) on the shared server change under them (ci-23). It empties the
// exclusive server's pg_stat_statements first: earlier holders' rows fill
// the cluster-wide hash, and near pg_stat_statements.max the collector
// reads differently. A server without the library fails here rather than
// skipping: the exclusive server is configured like CI's.
func exclusiveTextTestDB(t *testing.T, reason string) *pgxpool.Pool {
	t.Helper()
	pool := exclusivePool(t, reason)
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	mustExec(t, pool, "SELECT pg_stat_statements_reset()")
	return seedTextDB(t, pool)
}

// connectPool opens a pool on dsn for the rest of the test.
func connectPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping the test database: %v", err)
	}
	return pool
}

// ci-23: a measuring test starts from an empty pg_stat_statements hash on
// the exclusive server, whatever earlier holders left there: no row of
// another database survives, and the fixture's own seed rows are there.
// A hash near pg_stat_statements.max makes the collector classify texts
// and changes which rows the ranked arms admit. No concurrency test: the
// exclusive server is held by one test at a time.
func TestExclusiveTextTestDB_StartsFromAnEmptyStatementsHash(t *testing.T) {
	ctx := context.Background()
	pool := exclusiveTextTestDB(t, "an empty pg_stat_statements hash")
	var others, mine int64
	err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE dbid <> (SELECT oid FROM pg_database
			WHERE datname = current_database())),
		count(*) FILTER (WHERE dbid = (SELECT oid FROM pg_database
			WHERE datname = current_database()))
		FROM pg_stat_statements`).Scan(&others, &mine)
	if err != nil {
		t.Fatalf("count pg_stat_statements rows: %v", err)
	}
	if others != 0 || mine == 0 {
		t.Fatalf("pg_stat_statements rows: %d of other databases, %d of the fixture; "+
			"want none and some", others, mine)
	}
}
