package pressure

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// guard-01a on a real server (PG14 and PG17): the SQL source's bounded
// catalog reads, table resolution through to_regclass, the WAL rate and
// the lock queue as load rises and falls. A statement timeout on one input
// cannot be provoked on demand here (every read is a cheap catalog view);
// inputFailure and the checker's fake-source tests cover that rule.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/pressure"))
}

func dbPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	return testdb.Pool(t), context.Background()
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// fastChecker reads pool with a short cache so a test can step the load.
func fastChecker(pool *pgxpool.Pool, limits func(context.Context) (Limits, error)) *Checker {
	return NewChecker(Options{Database: "fixture", Source: NewSQLSource(pool, SQLOptions{}),
		CacheFor: 20 * time.Millisecond, WALFirstSpan: 100 * time.Millisecond, Limits: limits})
}

func TestPressureDB_IdleServerProceedsWithEveryInputKnown(t *testing.T) {
	pool, ctx := dbPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_idle (id int)")
	v, err := fastChecker(pool, nil).Check(ctx, Request{Table: "public.pressure_idle"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if v.Verdict != Proceed {
		t.Fatalf("idle fixture = %s %v, want proceed", v.Verdict, v.Reasons)
	}
	for _, name := range []string{policy.PressureXIDAge, policy.PressureMultixactAge,
		policy.PressureVacuum, policy.PressureWALRate, policy.PressureLockQueue} {
		in := inputOf(t, v, name)
		if in.State != StateKnown || in.Value == nil || in.ObservedAt == nil {
			t.Errorf("%s = %+v, want known with a value and its time", name, in)
		}
	}
	if xid := inputOf(t, v, policy.PressureXIDAge); *xid.Value <= 0 || xid.SlowLimit == nil {
		t.Errorf("xid age = %+v, want a positive age and the freeze maximum as slow", xid)
	}
	lag := inputOf(t, v, policy.PressureReplicaLag)
	if lag.State != StateNotApplicable || lag.Reason != "no replicas" {
		t.Errorf("replica lag without replicas = %+v, want not_applicable", lag)
	}
	if s := inputOf(t, v, policy.PressureFreeStorage); s.State != StateUnknown ||
		s.Reason == "" {
		t.Errorf("free storage without an adapter = %+v, want unknown with a reason", s)
	}
}

func TestPressureDB_TableNamesResolveThroughRegclass(t *testing.T) {
	pool, ctx := dbPool(t)
	exec(t, pool, `CREATE TABLE IF NOT EXISTS public."o'brien" (id int)`)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_victim (id int)")
	c := fastChecker(pool, nil)
	if v, err := c.Check(ctx, Request{Table: `public."o'brien"`}); err != nil ||
		v.Verdict != Proceed {
		t.Fatalf("quoted table = %v %v, want proceed", v.Verdict, err)
	}
	// PG14 raises "invalid name syntax" where PG16+ returns NULL; both are
	// an unknown table, so the answer does not depend on the version.
	for _, name := range []string{"x'; DROP TABLE pressure_victim; --", "public.missing",
		`"unbalanced`, "a.b.c.d"} {
		if _, err := c.Check(ctx, Request{Table: name}); !errors.Is(err, ErrUnknownTable) {
			t.Fatalf("table %q err = %v, want ErrUnknownTable", name, err)
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.pressure_victim') IS NOT NULL").
		Scan(&exists); err != nil || !exists {
		t.Fatalf("pressure_victim exists = %v (%v): the name was interpolated", exists, err)
	}
}

func TestPressureDB_WALRateOnTheSingleServer(t *testing.T) {
	pool, ctx := dbPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_wal (id int, pad text)")
	low := func(context.Context) (Limits, error) {
		l := DefaultLimits(Alerts{})
		l.Inputs[policy.PressureWALRate] = Limit{Slow: f(1024), Stop: f(1 << 40)}
		return l, nil
	}
	c := fastChecker(pool, low)
	if _, err := c.Check(ctx, Request{Table: "pressure_wal"}); err != nil {
		t.Fatalf("first check: %v", err)
	}
	exec(t, pool, "INSERT INTO pressure_wal SELECT g, repeat('w', 500) "+
		"FROM generate_series(1, 20000) g")
	time.Sleep(50 * time.Millisecond)
	v, err := c.Check(ctx, Request{Table: "pressure_wal"})
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	wal := inputOf(t, v, policy.PressureWALRate)
	if wal.State != StateKnown || *wal.Value < 1024 || wal.Band != BandSlow ||
		v.Verdict != Slow {
		t.Fatalf("WAL rate after ~10 MB of inserts = %+v, verdict %s; want slow over 1 KiB/s",
			wal, v.Verdict)
	}
}

// lockWaiters holds ACCESS EXCLUSIVE on table and queues readers behind it.
type lockWaiters struct {
	t      *testing.T
	pool   *pgxpool.Pool
	table  string
	holder pgx.Tx
	pids   []int
	wg     sync.WaitGroup
}

func holdLock(t *testing.T, pool *pgxpool.Pool, table string) *lockWaiters {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := tx.Exec(context.Background(), "LOCK TABLE "+table+
		" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	w := &lockWaiters{t: t, pool: pool, table: table, holder: tx}
	t.Cleanup(w.release)
	return w
}

// add queues one reader and waits until pg_locks shows it waiting.
func (w *lockWaiters) add() {
	w.t.Helper()
	conn, err := w.pool.Acquire(context.Background())
	if err != nil {
		w.t.Fatalf("acquire waiter: %v", err)
	}
	var pid int
	if err := conn.QueryRow(context.Background(), "SELECT pg_backend_pid()").
		Scan(&pid); err != nil {
		w.t.Fatalf("waiter pid: %v", err)
	}
	w.pids = append(w.pids, pid)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer conn.Release()
		_, _ = conn.Exec(context.Background(), "SELECT count(*) FROM "+w.table)
	}()
	w.await(len(w.pids))
}

// drop cancels the newest waiter.
func (w *lockWaiters) drop() {
	w.t.Helper()
	pid := w.pids[len(w.pids)-1]
	w.pids = w.pids[:len(w.pids)-1]
	exec(w.t, w.pool, "SELECT pg_cancel_backend($1)", pid)
	w.await(len(w.pids))
}

func (w *lockWaiters) await(n int) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
			WHERE relation = to_regclass($1) AND NOT granted`, w.table).Scan(&got); err != nil {
			w.t.Fatalf("count waiters: %v", err)
		}
		if got == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("waiters on %s never reached %d", w.table, n)
}

func (w *lockWaiters) release() {
	_ = w.holder.Rollback(context.Background())
	w.wg.Wait()
}

func TestPressureDB_LockQueueStateTransitions(t *testing.T) {
	pool, ctx := dbPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_lq (id int)")
	c := fastChecker(pool, nil)
	step := func(want Kind, waiters float64) {
		t.Helper()
		time.Sleep(30 * time.Millisecond) // past the cache period
		v, err := c.Check(ctx, Request{Table: "public.pressure_lq"})
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		lq := inputOf(t, v, policy.PressureLockQueue)
		if v.Verdict != want || lq.Value == nil || *lq.Value != waiters {
			t.Fatalf("with %v waiters = %s (lock queue %+v), want %s", waiters, v.Verdict,
				lq, want)
		}
	}
	step(Proceed, 0)
	w := holdLock(t, pool, "public.pressure_lq")
	w.add()
	step(Slow, 1)
	w.add()
	w.add()
	step(Stop, 3)
	w.drop()
	w.drop()
	step(Slow, 1)
	w.drop()
	step(Proceed, 0)
}

// countingSource counts the SQL source's read sets.
type countingSource struct {
	Source
	reads atomic.Int32
}

func (c *countingSource) Read(ctx context.Context, table string) (Readings, error) {
	c.reads.Add(1)
	return c.Source.Read(ctx, table)
}

func TestPressureDB_ConcurrentCallsMakeOneReadSet(t *testing.T) {
	pool, ctx := dbPool(t)
	exec(t, pool, "CREATE TABLE IF NOT EXISTS pressure_conc (id int)")
	src := &countingSource{Source: NewSQLSource(pool, SQLOptions{})}
	c := NewChecker(Options{Database: "fixture", Source: src})
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Check(ctx, Request{Table: "pressure_conc"}); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 || src.reads.Load() != 1 {
		t.Fatalf("100 concurrent calls: %d failed, %d read sets; want 0 and 1 per 2 s",
			failed.Load(), src.reads.Load())
	}
}
