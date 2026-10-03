package testdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent-access test: FlushStats acts on the one session it is
// given and keeps no state.

func flushConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// A session that reported its statistics less than the flush interval
// ago (1 s on PostgreSQL 15+, 500 ms on 14) holds its next counts until
// it is idle again after the interval; FlushStats makes them visible to
// any new reader at once.
func TestFlushStatsMakesPendingCountsVisible(t *testing.T) {
	dsn := CreateDatabase(t, "flush")
	ctx := context.Background()
	writer := flushConn(t, dsn)
	if _, err := writer.Exec(ctx, `CREATE TABLE public.flushed (id int)
		WITH (autovacuum_enabled = off)`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the writer's next idle report is due
	for _, sql := range []string{"SELECT 1", // reports, restarting the interval
		"INSERT INTO public.flushed SELECT generate_series(1, 1000)"} {
		if _, err := writer.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := FlushStats(ctx, writer); err != nil {
		t.Fatalf("FlushStats: %v", err)
	}
	var inserted int64
	if err := flushConn(t, dsn).QueryRow(ctx, `SELECT n_tup_ins FROM pg_stat_user_tables
		WHERE relid = 'public.flushed'::regclass`).Scan(&inserted); err != nil {
		t.Fatalf("read n_tup_ins: %v", err)
	}
	if inserted != 1000 {
		t.Fatalf("n_tup_ins = %d right after FlushStats, want 1000", inserted)
	}
}

func TestFlushStatsWithNothingPendingReturns(t *testing.T) {
	conn := flushConn(t, CreateDatabase(t, "flush_idle"))
	if err := FlushStats(context.Background(), conn); err != nil {
		t.Fatalf("FlushStats on an idle session: %v", err)
	}
}

func TestFlushStatsStopsOnACancelledContext(t *testing.T) {
	conn := flushConn(t, CreateDatabase(t, "flush_cancel"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := FlushStats(ctx, conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFlushStatsReportsAClosedSession(t *testing.T) {
	conn := flushConn(t, CreateDatabase(t, "flush_closed"))
	_ = conn.Close(context.Background())
	err := FlushStats(context.Background(), conn)
	if err == nil {
		t.Fatal("FlushStats on a closed session succeeded")
	}
}

// Every idle session of the pool reports what it holds.
func TestFlushIdleSessionsFlushesEveryIdleSession(t *testing.T) {
	dsn := CreateDatabase(t, "flush_pool")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.flushed (id int)
		WITH (autovacuum_enabled = off)`); err != nil {
		t.Fatal(err)
	}
	writers := []*pgxpool.Conn{}
	for range 2 {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		writers = append(writers, c)
	}
	time.Sleep(1100 * time.Millisecond) // each writer's next idle report is due
	for _, w := range writers {
		for _, sql := range []string{"SELECT 1",
			"INSERT INTO public.flushed SELECT generate_series(1, 500)"} {
			if _, err := w.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		w.Release()
	}
	if err := FlushIdleSessions(ctx, pool); err != nil {
		t.Fatalf("FlushIdleSessions: %v", err)
	}
	var inserted int64
	if err := flushConn(t, dsn).QueryRow(ctx, `SELECT n_tup_ins FROM pg_stat_user_tables
		WHERE relid = 'public.flushed'::regclass`).Scan(&inserted); err != nil {
		t.Fatalf("read n_tup_ins: %v", err)
	}
	if inserted != 1000 {
		t.Fatalf("n_tup_ins = %d right after FlushIdleSessions, want 1000", inserted)
	}
}
