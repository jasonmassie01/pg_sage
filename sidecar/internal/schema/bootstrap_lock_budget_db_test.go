package schema

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// config-04 on a real server: a second boot reads the catalog instead of
// re-running DDL, every bootstrap statement waits at most lock_timeout for
// a lock, and the sage.config key index is verified, not assumed.

// holdLocks opens a session holding mode on the sage tables (relkind r, p;
// only on table when it is set) until the test ends.
func holdLocks(t *testing.T, pool *pgxpool.Pool, mode, table string) (pgx.Tx, uint32) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock holder: %v", err)
	}
	t.Cleanup(conn.Release)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	rows, err := tx.Query(ctx, `SELECT c.oid::regclass::text FROM pg_class c
		WHERE c.relnamespace = 'sage'::regnamespace AND c.relkind IN ('r', 'p')
		  AND ($1 = '' OR c.relname = $1)`, table)
	if err != nil {
		t.Fatalf("list sage tables: %v", err)
	}
	rels, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(rels) == 0 {
		t.Fatalf("sage tables %v: %v", rels, err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE "+strings.Join(rels, ", ")+
		" IN "+mode+" MODE"); err != nil {
		t.Fatalf("lock %d sage tables in %s: %v", len(rels), mode, err)
	}
	var pid uint32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("lock holder pid: %v", err)
	}
	return tx, pid
}

// watchDDLLocks samples pg_locks until stop is closed and returns the locks
// on sage tables, held or awaited by any session but skip, in a mode that
// blocks writers or readers (a DDL lock).
func watchDDLLocks(t *testing.T, pool *pgxpool.Pool, skip uint32) func() []string {
	t.Helper()
	stop := make(chan struct{})
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := pool.Query(context.Background(), `
SELECT l.relation::regclass::text || ' ' || l.mode
  FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
 WHERE l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND c.relnamespace = 'sage'::regnamespace AND c.relkind IN ('r', 'p')
   AND l.pid <> $1 AND l.pid <> pg_backend_pid()
   AND l.mode NOT IN ('AccessShareLock', 'RowShareLock', 'RowExclusiveLock')`, skip)
			if err != nil {
				continue
			}
			found, _ := pgx.CollectRows(rows, pgx.RowTo[string])
			mu.Lock()
			for _, f := range found {
				seen[f] = true
			}
			mu.Unlock()
		}
	}()
	return func() []string {
		close(stop)
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for k := range seen {
			out = append(out, k)
		}
		return out
	}
}

// Happy path, the pg_dump case: a second boot while another session holds
// ACCESS SHARE on every sage table finishes at once and takes no DDL lock.
// Before config-04 the first ALTER queued behind the dump until the boot
// deadline.
func TestBootstrap_SecondBootUnderAccessShareTakesNoDDLLock(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_dump")
	_, holder := holdLocks(t, pool, "ACCESS SHARE", "")
	collect := watchDDLLocks(t, pool, holder)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := Bootstrap(ctx, pool)
	took := time.Since(start)
	ddl := collect()
	if err != nil {
		t.Fatalf("second boot under ACCESS SHARE: %v", err)
	}
	if took >= migrationLockTimeout {
		t.Errorf("second boot took %s, want under %s (no lock wait)", took,
			migrationLockTimeout)
	}
	if len(ddl) > 0 {
		t.Errorf("second boot took DDL locks on sage tables: %v", ddl)
	}
}

// Boundary: a step that really adds a column, blocked by ACCESS EXCLUSIVE,
// retries under lock_timeout, then fails naming the step, within its 10 s
// budget, and releases the advisory lock; once the lock is gone it applies.
func TestBootstrap_BlockedStepRetriesThenFailsNamingIt(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_blocked")
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		"ALTER TABLE sage.action_log DROP COLUMN approved_by"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	holder, _ := holdLocks(t, pool, "ACCESS EXCLUSIVE", "action_log")

	bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	err := Bootstrap(bctx, pool)
	took := time.Since(start)
	if err == nil || !isLockNotAvailable(err) {
		t.Fatalf("blocked boot: err = %v, want a lock_timeout failure", err)
	}
	for _, want := range []string{"ddlActionLogApprovalCols", "lock_timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if took < 2*migrationLockTimeout || took > migrationStepTimeout+2*time.Second {
		t.Errorf("blocked boot failed after %s, want at least two lock waits and "+
			"within the %s step budget", took, migrationStepTimeout)
	}
	probe, err := acquireAdvisoryLock(ctx, pool, 2*time.Second)
	if err != nil {
		t.Fatalf("advisory lock still held after the failed boot: %v", err)
	}
	if err := probe.Release(ctx); err != nil {
		t.Fatalf("release probe: %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release ACCESS EXCLUSIVE: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot after the lock went away: %v", err)
	}
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
		WHERE attrelid = 'sage.action_log'::regclass AND attname = 'approved_by'
		  AND NOT attisdropped)`).Scan(&restored); err != nil || !restored {
		t.Fatalf("approved_by restored = %t (%v), want true", restored, err)
	}
}

// The pooled session the boot used comes back with lock_timeout reset.
func TestBootstrap_ResetsLockTimeoutBeforeReturningTheSession(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_reset")
	one := singleConnPool(t, pool)
	if err := Bootstrap(context.Background(), one); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var setting string
	if err := one.QueryRow(context.Background(), "SHOW lock_timeout").
		Scan(&setting); err != nil || setting != "0" {
		t.Fatalf("lock_timeout after boot = %q (%v), want 0", setting, err)
	}
}

// Error propagation: a reset that fails closes the session instead of
// returning it to the pool with lock_timeout set.
func TestBootstrap_FailedResetDiscardsTheSession(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_discard")
	one := singleConnPool(t, pool)
	ctx := context.Background()
	var before uint32
	if err := one.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	saved := bootstrapResetLockTimeoutSQL
	bootstrapResetLockTimeoutSQL = "RESET no_such_setting_config04"
	t.Cleanup(func() { bootstrapResetLockTimeoutSQL = saved })
	err := Bootstrap(ctx, one)
	if err == nil || !strings.Contains(err.Error(), "reset lock_timeout") {
		t.Fatalf("Bootstrap with a failing reset: err = %v, want the reset error", err)
	}
	var after uint32
	var setting string
	if err := one.QueryRow(ctx, "SELECT pg_backend_pid(), current_setting('lock_timeout')").
		Scan(&after, &setting); err != nil {
		t.Fatal(err)
	}
	if after == before || setting != "0" {
		t.Fatalf("session %d -> %d with lock_timeout %q: want a new session at 0",
			before, after, setting)
	}
}

func singleConnPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.MinConns, cfg.MaxConns = 0, 1
	one, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("single-connection pool: %v", err)
	}
	t.Cleanup(one.Close)
	return one
}

// Error propagation: duplicate (key, database_id) rows keep the key index
// from being built; Bootstrap fails naming it and readiness stays red.
func TestBootstrap_DuplicateConfigKeysFailNamingTheIndex(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_dupkeys")
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP INDEX sage.idx_config_key_db",
		`INSERT INTO sage.config (key, value) VALUES ('dup_key', 'a'), ('dup_key', 'b')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	err := Bootstrap(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "idx_config_key_db") {
		t.Fatalf("Bootstrap over duplicates: err = %v, want it to name idx_config_key_db", err)
	}
	ready := CheckReady(ctx, pool)
	if !errors.Is(ready, ErrSchemaNotReady) || !strings.Contains(ready.Error(),
		"idx_config_key_db") {
		t.Fatalf("CheckReady = %v, want ErrSchemaNotReady naming the index", ready)
	}
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.config WHERE key = 'dup_key' AND value = 'b'"); err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap after removing the duplicate: %v", err)
	}
	if err := CheckReady(ctx, pool); err != nil {
		t.Fatalf("CheckReady after the index was rebuilt: %v", err)
	}
}

// Invalid state: an index of that name that is invalid or not unique is
// reported, not trusted.
func TestBootstrap_RejectsAnInvalidOrNonUniqueConfigKeyIndex(t *testing.T) {
	cases := map[string][]string{
		"invalid": {`UPDATE pg_index SET indisvalid = false
			WHERE indexrelid = 'sage.idx_config_key_db'::regclass`},
		"not unique": {"DROP INDEX sage.idx_config_key_db",
			"CREATE INDEX idx_config_key_db ON sage.config (key)"},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			pool := freshSchemaPool(t, "schema_boot_badidx")
			ctx := context.Background()
			for _, stmt := range setup {
				if _, err := pool.Exec(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			err := Bootstrap(ctx, pool)
			if err == nil || !strings.Contains(err.Error(),
				"idx_config_key_db is not a valid unique index") {
				t.Fatalf("Bootstrap: err = %v, want the index named", err)
			}
		})
	}
}
