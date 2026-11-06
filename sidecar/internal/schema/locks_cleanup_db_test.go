package schema

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-01 cleanup: the locks rows stored before the wait graph (raw
// session SQL, literals included, every lock of every session) are deleted
// once, in bounded batches under lock_timeout; rows written after the
// cleanup started are kept, and a finished cleanup never runs again.

// No concurrent-writer test beyond the lock wait below: Bootstrap holds
// its advisory lock, so one cleanup runs per database at a time.

func freshSchemaPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for run := range 2 { // a fresh server bootstraps twice cleanly
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", run, err)
		}
	}
	return pool
}

// seedLegacyLocks inserts n old-format locks rows (raw text with a
// literal) collected age ago, plus one row of another category.
func seedLegacyLocks(t *testing.T, pool *pgxpool.Pool, n int, age string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots
		(collected_at, category, data)
		SELECT now() - $2::text::interval, 'locks',
		       jsonb_build_array(jsonb_build_object('pid', g, 'locktype', 'relation',
		         'query', 'SELECT 1 WHERE id = ''secret-123'''))
		  FROM generate_series(1, $1) g`, n, age); err != nil {
		t.Fatalf("seed locks: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots
		(collected_at, category, data)
		VALUES (now() - $1::text::interval, 'system', '{"active_backends": 1}')`, age); err != nil {
		t.Fatalf("seed system: %v", err)
	}
}

// resetCleanup forgets the cleanup ran, as on a database upgraded from a
// version without it.
func resetCleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM sage.data_cleanup WHERE name = $1", locksCleanupName); err != nil {
		t.Fatalf("reset cleanup marker: %v", err)
	}
}

func countCategory(t *testing.T, pool *pgxpool.Pool, category string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.snapshots WHERE category = $1", category).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", category, err)
	}
	return n
}

func cleanupDone(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var done bool
	err := pool.QueryRow(context.Background(), `SELECT completed_at IS NOT NULL
		FROM sage.data_cleanup WHERE name = $1`, locksCleanupName).Scan(&done)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("read cleanup marker: %v", err)
	}
	return done
}

// More rows than one batch are all deleted; other categories are kept; a
// second bootstrap is a no-op and keeps rows written after the cleanup.
func TestLocksCleanup_DeletesLegacyRowsOnceInBatches(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "locks_cleanup")
	resetCleanup(t, pool)
	seedLegacyLocks(t, pool, locksCleanupBatch+5, "2 days")
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n := countCategory(t, pool, "locks"); n != 0 {
		t.Fatalf("%d locks rows remain, want 0", n)
	}
	if n := countCategory(t, pool, "system"); n != 1 {
		t.Fatalf("%d system rows remain, want the 1 seeded", n)
	}
	if !cleanupDone(t, pool) {
		t.Fatal("cleanup not marked complete")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (category, data)
		VALUES ('locks', '[]')`); err != nil {
		t.Fatal(err)
	}
	seedLegacyLocks(t, pool, 3, "3 days") // a late legacy row: retention ages it
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if n := countCategory(t, pool, "locks"); n != 4 {
		t.Fatalf("%d locks rows after a second bootstrap, want 4 (a finished "+
			"cleanup is a no-op)", n)
	}
}

// Rows collected after the cleanup first started (the new collector's) are
// never deleted, even when an interrupted cleanup resumes later.
func TestLocksCleanup_KeepsRowsAfterItsCutoff(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "locks_cutoff")
	resetCleanup(t, pool)
	seedLegacyLocks(t, pool, 10, "1 day")
	if _, err := pool.Exec(ctx, `INSERT INTO sage.data_cleanup (name, cutoff)
		VALUES ($1, now() - interval '1 hour')`, locksCleanupName); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (category, data)
		VALUES ('locks', '[]')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateLocksCleanup(ctx, pool); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := countCategory(t, pool, "locks"); n != 1 {
		t.Fatalf("%d locks rows remain, want the 1 written after the cutoff", n)
	}
}

// A cleanup that cannot get its lock in time stops without failing the
// bootstrap, stays unfinished and completes on the next run.
func TestLocksCleanup_LockTimeoutDefersWithoutFailing(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "locks_locked")
	resetCleanup(t, pool)
	seedLegacyLocks(t, pool, 5, "1 day")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.snapshots IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = migrateLocksCleanup(ctx, pool)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("cleanup under a held lock = %v, want deferred (nil)", err)
	}
	if took := time.Since(start); took > 2*locksCleanupBudget {
		t.Fatalf("deferred cleanup took %v, want it bounded by lock_timeout", took)
	}
	if cleanupDone(t, pool) || countCategory(t, pool, "locks") != 5 {
		t.Fatal("a deferred cleanup must leave its rows and stay unfinished")
	}
	if err := migrateLocksCleanup(ctx, pool); err != nil {
		t.Fatalf("resumed cleanup: %v", err)
	}
	if !cleanupDone(t, pool) || countCategory(t, pool, "locks") != 0 {
		t.Fatal("the resumed cleanup must finish and delete the rows")
	}
}

// Boundary: a budget spent after one batch leaves the rest for the next
// run, which resumes and finishes.
func TestLocksCleanup_BudgetResumes(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "locks_budget")
	resetCleanup(t, pool)
	seedLegacyLocks(t, pool, locksCleanupBatch+1, "1 day")
	old := locksCleanupBudget
	locksCleanupBudget = 0
	t.Cleanup(func() { locksCleanupBudget = old })
	if err := migrateLocksCleanup(ctx, pool); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := countCategory(t, pool, "locks"); n != 1 || cleanupDone(t, pool) {
		t.Fatalf("%d rows remain (done %v), want one batch deleted and the run unfinished",
			n, cleanupDone(t, pool))
	}
	locksCleanupBudget = old
	if err := migrateLocksCleanup(ctx, pool); err != nil {
		t.Fatalf("resumed cleanup: %v", err)
	}
	if n := countCategory(t, pool, "locks"); n != 0 || !cleanupDone(t, pool) {
		t.Fatalf("%d rows remain (done %v), want 0 and finished", n, cleanupDone(t, pool))
	}
}

// Error propagation: only a lock or statement timeout defers the cleanup;
// any other failure is the bootstrap's error.
func TestLocksCleanupDeferrable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "55P03"}, true},
		{&pgconn.PgError{Code: "57014"}, true},
		{&pgconn.PgError{Code: "42501"}, false},
		{errors.New("connection reset"), false},
		{nil, false},
	} {
		if got := locksCleanupDeferrable(tc.err); got != tc.want {
			t.Errorf("deferrable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Error propagation: a permission failure is returned with context.
func TestLocksCleanup_PermissionErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	pool := freshSchemaPool(t, "locks_perm")
	resetCleanup(t, pool)
	if _, err := pool.Exec(ctx, `DROP ROLE IF EXISTS locks_cleanup_ro;
		CREATE ROLE locks_cleanup_ro;
		GRANT USAGE, CREATE ON SCHEMA sage TO locks_cleanup_ro;
		GRANT SELECT, INSERT, UPDATE ON sage.data_cleanup TO locks_cleanup_ro;
		GRANT SELECT ON sage.snapshots TO locks_cleanup_ro`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP OWNED BY locks_cleanup_ro;
			DROP ROLE IF EXISTS locks_cleanup_ro`)
	})
	seedLegacyLocks(t, pool, 1, "1 day")
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE locks_cleanup_ro"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
	err = migrateLocksCleanup(ctx, conn)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("cleanup without DELETE = %v, want a permission error", err)
	}
}
