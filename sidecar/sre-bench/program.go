package srebench

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// program is a fault program built from optional steps.
type program struct {
	inject, manifest, between, valid, recover func(ctx context.Context, e *Env) error
}

func step(f func(context.Context, *Env) error, ctx context.Context, e *Env) error {
	if f == nil {
		return nil
	}
	return f(ctx, e)
}

// Inject creates the fault.
func (p program) Inject(ctx context.Context, e *Env) error { return step(p.inject, ctx, e) }

// Manifest checks the fault is present before the investigation.
func (p program) Manifest(ctx context.Context, e *Env) error { return step(p.manifest, ctx, e) }

// Between runs at the start of the sample interval.
func (p program) Between(ctx context.Context, e *Env) error { return step(p.between, ctx, e) }

// Valid checks the scenario's premise held through the investigation.
func (p program) Valid(ctx context.Context, e *Env) error { return step(p.valid, ctx, e) }

// Recover removes the fault and checks it is gone.
func (p program) Recover(ctx context.Context, e *Env) error { return step(p.recover, ctx, e) }

// table is a scenario's own table.
type table struct{ name string }

// nameSeq keeps generated names distinct within a process.
var nameSeq atomic.Int64

// uniqueName is prefix followed by a process-unique suffix.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), nameSeq.Add(1))
}

func newTable(prefix string) *table {
	return &table{name: pgx.Identifier{uniqueName("bench_" + prefix + "_")}.Sanitize()}
}

func (tb *table) create(ctx context.Context, e *Env) error {
	_, err := e.Pool.Exec(ctx, "CREATE TABLE "+tb.name+
		" (id int PRIMARY KEY, v int); INSERT INTO "+tb.name+" VALUES (1, 0)")
	return err
}

// drop removes the table and checks no lock wait is left behind.
func (tb *table) drop(ctx context.Context, e *Env) error {
	if _, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+tb.name); err != nil {
		return err
	}
	return waitFor(ctx, "lock waits to clear", func() (bool, error) {
		n, err := e.lockWaiters(ctx)
		return n == 0, err
	})
}

// sessionState waits until pid reaches state.
func (e *Env) sessionState(ctx context.Context, pid int, state string) error {
	return waitFor(ctx, fmt.Sprintf("pid %d %s", pid, state), func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE pid = $1 AND state = $2`, pid, state)
		return n == 1, err
	})
}

// atLeastWaiters waits until n sessions wait on locks.
func (e *Env) atLeastWaiters(ctx context.Context, n int) error {
	return waitFor(ctx, fmt.Sprintf("%d lock waiters", n), func() (bool, error) {
		got, err := e.lockWaiters(ctx)
		return got >= n, err
	})
}

// sleeping waits until pid is in pg_sleep.
func (e *Env) sleeping(ctx context.Context, pid int) error {
	return waitFor(ctx, fmt.Sprintf("pid %d sleeping", pid), func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE pid = $1 AND wait_event = 'PgSleep'`, pid)
		return n == 1, err
	})
}
