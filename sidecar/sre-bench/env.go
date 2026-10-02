package srebench

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Env is the benchmark's disposable database: the investigator's store
// and probes run on it, and fault programs open their sessions on it.
type Env struct {
	DSN    string
	Pool   *pgxpool.Pool
	Store  *sre.PostgresStore
	Runner *probes.Runner

	mu       sync.Mutex
	sessions []*session
	release  func()
}

// NewEnv bootstraps the sage schema on dsn's database.
func NewEnv(ctx context.Context, t *testing.T, dsn string) *Env {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect bench database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap bench database: %v", err)
	}
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return &Env{DSN: dsn, Pool: pool, Store: st,
		Runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))}
}

// session is a background connection a fault program holds open. err
// is its statement's outcome, readable once done is closed.
type session struct {
	conn *pgx.Conn
	pid  int
	done chan struct{}
	err  error
}

// connect opens a tracked connection with an application name.
func (e *Env) connect(ctx context.Context, app string) (*session, error) {
	cfg, err := pgx.ParseConfig(e.DSN)
	if err != nil {
		return nil, err
	}
	if app != "" {
		cfg.RuntimeParams["application_name"] = app
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", app, err)
	}
	s := &session{conn: conn, done: make(chan struct{})}
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&s.pid); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	e.mu.Lock()
	e.sessions = append(e.sessions, s)
	e.mu.Unlock()
	return s, nil
}

// idle opens n idle connections of one application.
func (e *Env) idle(ctx context.Context, app string, n int) error {
	for i := 0; i < n; i++ {
		s, err := e.connect(ctx, app)
		if err != nil {
			return err
		}
		close(s.done)
	}
	return nil
}

// background runs sql (simple protocol, several statements allowed) on
// a new tracked session without waiting for it; it may block on a lock.
func (e *Env) background(ctx context.Context, app, sql string) (int, error) {
	s, err := e.connect(ctx, app)
	if err != nil {
		return 0, err
	}
	go func() {
		defer close(s.done)
		_, s.err = s.conn.PgConn().Exec(context.Background(), sql).ReadAll()
	}()
	return s.pid, nil
}

// closeSessions terminates every tracked session and waits for them.
func (e *Env) closeSessions() {
	e.mu.Lock()
	sessions := e.sessions
	e.sessions = nil
	e.mu.Unlock()
	for _, s := range sessions {
		_, _ = e.Pool.Exec(context.Background(), "SELECT pg_terminate_backend($1)", s.pid)
		<-s.done
		_ = s.conn.Close(context.Background())
	}
}

// waitFor polls cond until it holds or 15 s pass.
func waitFor(ctx context.Context, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		ok, err := cond()
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", what, err)
		case ok:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("%s: not reached in 15 s", what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// count runs a count query with args.
func (e *Env) count(ctx context.Context, sql string, args ...any) (int, error) {
	var n int
	err := e.Pool.QueryRow(ctx, sql, args...).Scan(&n)
	return n, err
}

// lockWaiters counts sessions of this database waiting on a lock.
func (e *Env) lockWaiters(ctx context.Context) (int, error) {
	return e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`)
}

// simple runs sql (several statements allowed) with the simple protocol
// on a pooled connection.
func (e *Env) simple(ctx context.Context, sql string) error {
	conn, err := e.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
	return err
}
