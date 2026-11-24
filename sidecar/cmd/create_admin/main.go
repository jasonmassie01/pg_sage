package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

type adminConfig struct {
	DSN      string
	Email    string
	Password string
}

type adminDeps struct {
	open          func(context.Context, string) (*pgxpool.Pool, error)
	close         func(*pgxpool.Pool)
	resetSequence func(context.Context, *pgxpool.Pool) error
	createUser    func(context.Context, *pgxpool.Pool, string, string, string) (int, error)
}

func main() {
	os.Exit(run(os.Getenv, os.Stderr, productionAdminDeps()))
}

// run is main without the process exit: it returns 1 on any error
// (apiauth-14).
func run(getenv func(string) string, stderr io.Writer, deps adminDeps) int {
	cfg, err := loadAdminConfig(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	id, err := createAdmin(context.Background(), cfg, deps)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr,
		"Created admin user id=%d\n  email: %s\n"+
			"  password: [set via PG_SAGE_ADMIN_PASSWORD]\n",
		id, cfg.Email,
	)
	return 0
}

func loadAdminConfig(getenv func(string) string) (adminConfig, error) {
	cfg := adminConfig{
		DSN: getenv("PG_SAGE_DSN"), Email: getenv("PG_SAGE_ADMIN_EMAIL"),
		Password: getenv("PG_SAGE_ADMIN_PASSWORD"),
	}
	if cfg.DSN == "" {
		return adminConfig{}, fmt.Errorf("PG_SAGE_DSN environment variable must be set")
	}
	if cfg.Password == "" {
		return adminConfig{}, fmt.Errorf(
			"PG_SAGE_ADMIN_PASSWORD environment variable must be set")
	}
	if len(cfg.Password) < 12 {
		return adminConfig{}, fmt.Errorf(
			"PG_SAGE_ADMIN_PASSWORD must be at least 12 characters")
	}
	if cfg.Email == "" {
		cfg.Email = "admin@pg-sage.local"
	}
	return cfg, nil
}

func createAdmin(
	ctx context.Context, cfg adminConfig, deps adminDeps,
) (int, error) {
	pool, err := deps.open(ctx, cfg.DSN)
	if err != nil {
		return 0, fmt.Errorf("connect to admin database: %w", err)
	}
	defer deps.close(pool)
	if err := deps.resetSequence(ctx, pool); err != nil {
		return 0, fmt.Errorf("reset sage.users sequence: %w", err)
	}
	id, err := deps.createUser(ctx, pool, cfg.Email, cfg.Password, "admin")
	if err != nil {
		return 0, fmt.Errorf("create admin user: %w", err)
	}
	return id, nil
}

// sequenceLockKey serializes create_admin runs on one database: a run holds
// it from setval until its insert has committed (apiauth-14).
const sequenceLockKey = "pg_sage.create_admin.users_id"

// advanceSequenceSQL moves the sage.users id sequence past the highest
// committed id (a restored dump can leave the sequence behind the data). It
// never moves it below the last id handed out, which another writer may hold
// in an open transaction (apiauth-14).
const advanceSequenceSQL = `SELECT setval(s.seq, GREATEST(
	COALESCE((SELECT max(id) FROM sage.users), 0),
	COALESCE(pg_sequence_last_value(s.seq), 0)) + 1, false)
FROM (SELECT pg_get_serial_sequence('sage.users', 'id')::regclass AS seq) s`

func productionAdminDeps() adminDeps {
	var held *pgxpool.Conn
	return adminDeps{
		open: openAdminPool,
		close: func(pool *pgxpool.Pool) {
			if held != nil {
				// Ending the session releases the advisory lock; a close
				// error leaves nothing to clean up.
				_ = held.Hijack().Close(context.Background())
				held = nil
			}
			pool.Close()
		},
		resetSequence: func(ctx context.Context, pool *pgxpool.Pool) error {
			conn, err := lockSequence(ctx, pool)
			if err != nil {
				return err
			}
			held = conn
			_, err = conn.Exec(ctx, advanceSequenceSQL)
			return err
		},
		createUser: auth.CreateUser,
	}
}

// minAdminConns is the pool size a run needs: one session holds the
// sequence lock while auth.CreateUser inserts on another. A DSN with
// pool_max_conns=1 would otherwise wait forever for the second.
const minAdminConns = 2

// openAdminPool connects and pings: a pool connects lazily, so an
// unreachable server would otherwise surface at the first query.
func openAdminPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if err := databricksauth.ConfigurePool(ctx, cfg); err != nil {
		return nil, err
	}
	if cfg.MaxConns < minAdminConns {
		cfg.MaxConns = minAdminConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// lockSequence takes the create_admin advisory lock on a dedicated session,
// waiting at most 30 seconds for another run to finish.
func lockSequence(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire a session for the sequence lock: %w", err)
	}
	_, err = conn.Exec(ctx, "SET lock_timeout = '30s'")
	if err == nil {
		_, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", sequenceLockKey)
	}
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock the sage.users sequence: %w", err)
	}
	return conn, nil
}
