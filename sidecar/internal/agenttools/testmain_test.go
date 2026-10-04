package agenttools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agenttools"))
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

// fixture is one test's view of the package's live fixture database: a
// pool with the sage schema bootstrapped (once per process) and a schema
// of its own for the tables the test creates, dropped when the test ends.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	dsn    string
	schema string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := openPool(t, ctx, dsn)
	bootstrapOnce.Do(func() { bootstrapErr = schema.Bootstrap(ctx, pool) })
	if bootstrapErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootstrapErr)
	}
	f := &fixture{t: t, ctx: ctx, pool: pool, dsn: dsn, schema: uniqueName("at_")}
	f.exec("CREATE SCHEMA " + f.schema)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+f.schema+" CASCADE")
		if err != nil {
			t.Errorf("drop test schema %s: %v", f.schema, err)
		}
	})
	return f
}

// openPool connects to dsn and closes the pool when the test ends.
func openPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// extraDatabase creates a second disposable database with the sage schema
// bootstrapped and returns a pool on it.
func extraDatabase(t *testing.T, ctx context.Context, label string) *pgxpool.Pool {
	t.Helper()
	pool := openPool(t, ctx, testdb.CreateDatabase(t, label))
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap extra database %s: %v", label, err)
	}
	return pool
}

// uniqueName is a lower-case identifier unique to this run.
func uniqueName(prefix string) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

// q is name qualified with the test's schema.
func (f *fixture) q(name string) string { return f.schema + "." + name }

// exec runs statements in order, failing the test on the first error.
func (f *fixture) exec(stmts ...string) {
	f.t.Helper()
	for _, s := range stmts {
		if _, err := f.pool.Exec(f.ctx, s); err != nil {
			f.t.Fatalf("%s: %v", s, err)
		}
	}
}

// execArgs runs one parameterized statement.
func (f *fixture) execArgs(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// mustExec runs sql on another pool (an extra database).
func mustExec(t *testing.T, f *fixture, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// scalar scans the single value sql returns into dest.
func (f *fixture) scalar(dest any, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(dest); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// relationExists reports whether the qualified relation is in the catalog.
func (f *fixture) relationExists(qualified string) bool {
	f.t.Helper()
	var ok bool
	f.scalar(&ok, "SELECT to_regclass($1) IS NOT NULL", qualified)
	return ok
}

// count returns SELECT count(*) of the given FROM/WHERE tail.
func (f *fixture) count(tail string, args ...any) int64 {
	f.t.Helper()
	var n int64
	f.scalar(&n, "SELECT count(*) FROM "+tail, args...)
	return n
}

// ordersTable creates and analyzes <schema>.orders with rows rows: status
// cycles over three values, customer_id has rows/5 distinct values.
func (f *fixture) ordersTable(rows int) string {
	f.t.Helper()
	name := f.q("orders")
	f.exec("CREATE TABLE "+name+" (id bigint PRIMARY KEY, customer_id int NOT NULL, "+
		"status text NOT NULL, amount numeric NOT NULL)",
		"INSERT INTO "+name+" SELECT i, i % greatest(1, "+strconv.Itoa(rows/5)+"), "+
			"(ARRAY['open','paid','void'])[1 + i % 3], i % 1000 FROM generate_series(1, "+
			strconv.Itoa(rows)+") i",
		"ANALYZE "+name)
	return name
}

// requirePGSS skips the test when pg_stat_statements cannot be read (not
// in shared_preload_libraries or the extension is missing).
func (f *fixture) requirePGSS() {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	if err == nil {
		_, err = f.pool.Exec(f.ctx, "SELECT 1 FROM pg_stat_statements LIMIT 1")
	}
	if err != nil {
		f.t.Skipf("pg_stat_statements unavailable on the test server: %v", err)
	}
}

// resetOwnStatements clears pg_stat_statements for the fixture database
// only, so the test's workload is not outranked by bootstrap statements.
// A database-scoped reset leaves pg_stat_statements_info.stats_reset alone
// and so does not disturb other packages' pgssepoch checks.
func (f *fixture) resetOwnStatements() {
	f.t.Helper()
	f.exec(`SELECT pg_stat_statements_reset(0, d.oid, 0)
		FROM pg_database d WHERE d.datname = current_database()`)
}

// statementID returns the queryid of the fixture database's
// pg_stat_statements entry whose text contains fragment.
func (f *fixture) statementID(fragment string) (int64, bool) {
	f.t.Helper()
	var id int64
	err := f.pool.QueryRow(f.ctx, `SELECT s.queryid FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database() AND strpos(s.query, $1) > 0
		  AND strpos(s.query, 'pg_stat_statements') = 0
		ORDER BY s.calls DESC LIMIT 1`, fragment).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		f.t.Fatalf("look up statement %q: %v", fragment, err)
	}
	return id, true
}

// fixedClock is a Now func pinned at at.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// toFloat converts a JSON-decoded or Go numeric value to float64.
func toFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	}
	t.Fatalf("value %v (%T) is not a number", v, v)
	return 0
}
