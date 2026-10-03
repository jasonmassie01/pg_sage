package catalogread

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The shared bounded-read helper: every read-only analysis statement on
// the monitored database runs in its own read-only transaction under the
// configured statement and lock timeouts, without parallel workers or JIT
// (perf fix phase; static.md F11: most such paths had no server timeout).

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestFromSafety(t *testing.T) {
	got := FromSafety(config.SafetyConfig{QueryTimeoutMs: 750, LockTimeoutMs: 2000})
	if got.Statement != 750*time.Millisecond || got.Lock != 2*time.Second {
		t.Fatalf("FromSafety = %+v", got)
	}
	zero := FromSafety(config.SafetyConfig{})
	defaultLock := time.Duration(config.DefaultLockTimeoutMs) * time.Millisecond
	if zero.Statement != 0 || zero.Lock != defaultLock {
		t.Fatalf("zero safety = %+v, want no statement limit and the default lock timeout", zero)
	}
	d := Default()
	if d.Statement != time.Duration(config.DefaultQueryTimeoutMs)*time.Millisecond ||
		d.Lock != time.Duration(config.DefaultLockTimeoutMs)*time.Millisecond {
		t.Fatalf("Default = %+v, want the config defaults", d)
	}
}

func settingsIn(t *testing.T, r Reader) []string {
	t.Helper()
	var st, lt, par, jit, ro string
	if err := r.QueryRow(context.Background(), `SELECT current_setting('statement_timeout'),
		current_setting('lock_timeout'), current_setting('max_parallel_workers_per_gather'),
		current_setting('jit'), current_setting('transaction_read_only')`).
		Scan(&st, &lt, &par, &jit, &ro); err != nil {
		t.Fatalf("settings: %v", err)
	}
	return []string{st, lt, par, jit, ro}
}

func TestReader_BoundedReadOnlyTransaction(t *testing.T) {
	pool := testPool(t)
	r := New(pool, Timeouts{Statement: 750 * time.Millisecond, Lock: 3 * time.Second})
	if got := strings.Join(settingsIn(t, r), ","); got != "750ms,3s,0,off,on" {
		t.Fatalf("settings = %s, want 750ms,3s,0,off,on", got)
	}
	unlimited := New(pool, Timeouts{})
	if got := settingsIn(t, unlimited); got[0] != "0" || got[1] != "0" {
		t.Fatalf("zero timeouts = %v, want no limits", got)
	}
	var after string
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('statement_timeout')").Scan(&after); err != nil || after == "750ms" {
		t.Fatalf("setting leaked out of the transaction: %q (%v)", after, err)
	}
}

func TestReader_SlowStatementCutOffAtConfiguredTimeout(t *testing.T) {
	r := New(testPool(t), Timeouts{Statement: 200 * time.Millisecond})
	start := time.Now()
	rows, err := r.Query(context.Background(), "SELECT pg_sleep(5)")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("slow read error = %v, want a statement timeout (57014)", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("slow read took %s, want about the 200ms timeout", el)
	}
	err = r.QueryRow(context.Background(), "SELECT pg_sleep(5)").Scan()
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("slow QueryRow error = %v, want a statement timeout", err)
	}
}

func TestReader_RefusesWrites(t *testing.T) {
	pool := testPool(t)
	r := New(pool, Default())
	rows, err := r.Query(context.Background(), "CREATE TABLE catalogread_must_not_exist (a int)")
	if err == nil {
		rows.Close()
		err = rows.Err()
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write error = %v, want read-only transaction (25006)", err)
	}
}

func TestReader_CloseEndsTheTransaction(t *testing.T) {
	pool := testPool(t)
	r := New(pool, Default())
	for i := 0; i < 3; i++ {
		rows, err := r.Query(context.Background(), "SELECT generate_series(1, 3)")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		rows.Close()
		var n int
		if err := r.QueryRow(context.Background(), "SELECT 1").Scan(&n); err != nil || n != 1 {
			t.Fatalf("row: %d (%v)", n, err)
		}
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("%d connections still held after Close/Scan", acquired)
	}
}

func TestReader_BeforeStatementHookRunsInsideTheTransaction(t *testing.T) {
	r := New(testPool(t), Timeouts{Statement: 200 * time.Millisecond})
	calls := 0
	ctx := WithBeforeStatement(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		calls++
		_, err := tx.Exec(ctx, "SELECT pg_sleep(5)")
		return err
	})
	start := time.Now()
	_, err := r.Query(ctx, "SELECT 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" || calls != 1 {
		t.Fatalf("hooked read = %v after %d hook calls, want a statement timeout", err, calls)
	}
	if err := r.QueryRow(ctx, "SELECT 1").Scan(new(int)); !errors.As(err, &pgErr) || calls != 2 {
		t.Fatalf("hooked QueryRow = %v after %d calls", err, calls)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("hooked reads took %s", el)
	}
	if err := r.QueryRow(context.Background(), "SELECT 1").Scan(new(int)); err != nil {
		t.Fatalf("unhooked read after hooked ones: %v", err)
	}
}

func TestReader_AfterHookAndErrors(t *testing.T) {
	pool := testPool(t)
	var seen []string
	r := New(pool, Default())
	r.After = func(_ context.Context, _ pgx.Tx, sql string, args []any) {
		seen = append(seen, sql)
	}
	if err := r.QueryRow(context.Background(), "SELECT $1::int", 7).Scan(new(int)); err != nil {
		t.Fatalf("row: %v", err)
	}
	if len(seen) != 1 || seen[0] != "SELECT $1::int" {
		t.Fatalf("after hook saw %v", seen)
	}
	if _, err := r.Query(context.Background(), "SELECT * FROM no_such_table"); err == nil {
		t.Fatal("query of a missing table returned no error")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.QueryRow(canceled, "SELECT 1").Scan(new(int)); err == nil {
		t.Fatal("canceled context returned no error")
	}
	if _, err := (Reader{}).Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("a reader without a database returned no error")
	}
	if err := (Reader{}).QueryRow(context.Background(), "SELECT 1").Scan(); err == nil {
		t.Fatal("a reader without a database returned no row error")
	}
}

func TestReader_ConcurrentReads(t *testing.T) {
	r := New(testPool(t), Default())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var n int
			err := r.QueryRow(context.Background(), "SELECT $1::int", i).Scan(&n)
			if err == nil && n != i {
				err = errors.New("wrong row")
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	}
}
