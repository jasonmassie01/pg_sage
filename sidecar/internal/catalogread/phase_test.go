package catalogread

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A bounded read that times out says which phase spent the time: getting
// a connection and starting the transaction (pool_acquire), the server running
// the statement (server_execution) or waiting for a lock (lock_wait).

func TestRetryable_ServerTimeoutsOnly(t *testing.T) {
	stmt := &pgconn.PgError{Code: "57014"}
	lock := &pgconn.PgError{Code: "55P03"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"statement timeout", stmt, true},
		{"lock timeout", lock, true},
		{"wrapped statement timeout", fmt.Errorf("collect indexes: %w", stmt), true},
		{"phase-wrapped statement timeout",
			&PhaseError{Phase: PhaseExecution, Err: stmt}, true},
		{"missing table", &pgconn.PgError{Code: "42P01"}, false},
		{"permission", &pgconn.PgError{Code: "42501"}, false},
		{"client deadline in execution",
			&PhaseError{Phase: PhaseExecution, Err: context.DeadlineExceeded}, true},
		{"client deadline in begin",
			&PhaseError{Phase: PhaseBegin, Err: context.DeadlineExceeded}, false},
		{"canceled", context.Canceled, false},
		{"no rows", pgx.ErrNoRows, false},
		{"plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("%s: Retryable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPhaseError_MessageNamesPhaseAndKeepsCause(t *testing.T) {
	cause := &pgconn.PgError{Code: "57014", Message: "canceling statement due to " +
		"statement timeout"}
	err := &PhaseError{Phase: PhaseExecution, Begin: 4 * time.Millisecond,
		Execution: 503 * time.Millisecond, Err: cause}
	msg := err.Error()
	for _, want := range []string{"canceling statement due to statement timeout",
		"phase server_execution", "pool acquire 4 ms", "server execution 503 ms"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatal("the PostgreSQL error is no longer reachable through errors.As")
	}
}

// pgx.ErrNoRows and other non-timeout errors stay unwrapped: callers
// compare them directly.
func TestReader_NonTimeoutErrorsAreNotWrapped(t *testing.T) {
	r := New(testPool(t), Default())
	err := r.QueryRow(context.Background(), "SELECT 1 WHERE false").Scan(new(int))
	if err != pgx.ErrNoRows { //nolint:errorlint // callers compare it directly
		t.Fatalf("no-rows error = %#v, want pgx.ErrNoRows itself", err)
	}
	_, err = r.Query(context.Background(), "SELECT * FROM catalogread_no_such_table")
	var pe *PhaseError
	if errors.As(err, &pe) {
		t.Fatalf("missing-table error wrapped in a phase: %v", err)
	}
}

func TestReader_StatementTimeoutNamesServerExecution(t *testing.T) {
	r := New(testPool(t), Timeouts{Statement: 150 * time.Millisecond})
	rows, err := r.Query(context.Background(), "SELECT pg_sleep(5)")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhaseExecution {
		t.Fatalf("slow Query error = %v, want a server_execution phase error", err)
	}
	if pe.Execution < 100*time.Millisecond || pe.Execution > 3*time.Second {
		t.Fatalf("execution = %s, want about the 150 ms timeout", pe.Execution)
	}
	if !Retryable(err) {
		t.Fatal("a statement timeout must be retryable")
	}
	err = r.QueryRow(context.Background(), "SELECT pg_sleep(5)").Scan()
	if !errors.As(err, &pe) || pe.Phase != PhaseExecution {
		t.Fatalf("slow QueryRow error = %v, want a server_execution phase error", err)
	}
}

func TestReader_LockTimeoutNamesLockWait(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS catalogread_locked; "+
		"CREATE TABLE catalogread_locked (id int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE catalogread_locked") })
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, "LOCK TABLE catalogread_locked IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	r := New(pool, Timeouts{Statement: 2 * time.Second, Lock: 100 * time.Millisecond})
	err = r.QueryRow(ctx, "SELECT count(*) FROM catalogread_locked").Scan(new(int))
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhaseLockWait {
		t.Fatalf("locked read error = %v, want a lock_wait phase error", err)
	}
}

// A connection the pool cannot give before the caller's deadline fails
// in the pool_acquire phase, and is not retryable.
func TestReader_BusyPoolNamesBegin(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	held := make([]*pgxpool.Conn, 0)
	for i := int32(0); i < pool.Config().MaxConns; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold connection %d: %v", i, err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err := New(pool, Default()).QueryRow(short, "SELECT 1").Scan(new(int))
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhaseBegin || pe.Begin < 150*time.Millisecond {
		t.Fatalf("busy-pool read error = %v, want a pool_acquire phase error of ~200 ms", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || Retryable(err) {
		t.Fatalf("busy-pool error %v: want DeadlineExceeded, not retryable", err)
	}
}
