package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-04: an install whose sessions table predates token_hash gains the
// column on upgrade, every old-shape session (whose id was the cookie) is
// deleted, and a second run changes nothing.
func TestSessionsTokenHashMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	uid := sessionsMigrationUser(t, ctx, pool, "sessions-mig@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.users WHERE id = $1", uid)
	})
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.sessions DROP COLUMN token_hash`); err != nil {
		t.Fatalf("simulating the pre-hash schema: %v", err)
	}
	var legacy string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sessions (user_id, expires_at)
		VALUES ($1, now() + interval '1 hour') RETURNING id::text`, uid).Scan(&legacy); err != nil {
		t.Fatalf("inserting an old-shape session: %v", err)
	}

	bootstrapWithRetry(t, ctx, pool)
	if n := sessionRows(t, ctx, pool, "id::text = $1", legacy); n != 0 {
		t.Fatalf("old-shape session survived the migration (%d rows)", n)
	}
	requireTokenHashShape(t, ctx, pool)

	if _, err := pool.Exec(ctx, `INSERT INTO sage.sessions (user_id, expires_at, token_hash)
		VALUES ($1, now() + interval '1 hour', $2)`, uid, strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("inserting a hashed session: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool) // second run is a no-op
	if n := sessionRows(t, ctx, pool, "token_hash = $1", strings.Repeat("ab", 32)); n != 1 {
		t.Fatalf("re-running the migration removed a hashed session (%d rows left)", n)
	}
	requireTokenHashShape(t, ctx, pool)
}

// The documented downgrade step drops NOT NULL; an older sidecar then stores
// unhashed sessions. Upgrading again deletes them and restores the shape.
func TestSessionsTokenHashMigrationAfterDowngrade(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	uid := sessionsMigrationUser(t, ctx, pool, "sessions-downgrade@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.users WHERE id = $1", uid)
	})
	if _, err := pool.Exec(ctx,
		`ALTER TABLE sage.sessions ALTER COLUMN token_hash DROP NOT NULL`); err != nil {
		t.Fatalf("documented downgrade step: %v", err)
	}
	var legacy string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sessions (user_id, expires_at)
		VALUES ($1, now() + interval '1 hour') RETURNING id::text`, uid).Scan(&legacy); err != nil {
		t.Fatalf("inserting a session as the older sidecar would: %v", err)
	}
	keep := strings.Repeat("ef", 32)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sessions (user_id, expires_at, token_hash)
		VALUES ($1, now() + interval '1 hour', $2)`, uid, keep); err != nil {
		t.Fatalf("inserting a hashed session: %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	if n := sessionRows(t, ctx, pool, "id::text = $1", legacy); n != 0 {
		t.Fatalf("unhashed session survived the re-upgrade (%d rows)", n)
	}
	if n := sessionRows(t, ctx, pool, "token_hash = $1", keep); n != 1 {
		t.Fatalf("re-upgrade removed a hashed session (%d rows left)", n)
	}
	requireTokenHashShape(t, ctx, pool)
}

func TestSessionsTokenHashRejectsNullAndDuplicates(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	uid := sessionsMigrationUser(t, ctx, pool, "sessions-null@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.users WHERE id = $1", uid)
	})
	_, err := pool.Exec(ctx, `INSERT INTO sage.sessions (user_id, expires_at)
		VALUES ($1, now() + interval '1 hour')`, uid)
	requireSQLState(t, err, "23502", "an unhashed session")
	dup := strings.Repeat("cd", 32)
	for i := 0; i < 2; i++ {
		_, err = pool.Exec(ctx, `INSERT INTO sage.sessions (user_id, expires_at, token_hash)
			VALUES ($1, now() + interval '1 hour', $2)`, uid, dup)
	}
	requireSQLState(t, err, "23505", "a duplicate token hash")
}

// The first run of the migration is under lock_timeout: a session-table
// lock held by another backend makes it fail fast instead of stalling
// startup.
func TestSessionsTokenHashMigrationHonoursLockTimeout(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() { bootstrapWithRetry(t, context.Background(), pool) })
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.sessions DROP COLUMN token_hash`); err != nil {
		t.Fatalf("simulating the pre-hash schema: %v", err)
	}
	holder := holdSessionsShareLock(t, ctx, pool)
	defer func() { _ = holder.Rollback(context.Background()) }()
	start := time.Now()
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := pool.Exec(qctx, ddlSessionsTokenHash)
	requireSQLState(t, err, "55P03", "the migration behind a held lock")
	if waited := time.Since(start); waited > 15*time.Second {
		t.Fatalf("migration waited %v for the lock, want it bounded by lock_timeout", waited)
	}
}

// Once applied, a re-run (every restart) takes no lock on sage.sessions,
// so a held lock neither delays nor fails startup.
func TestSessionsTokenHashMigrationRerunTakesNoLock(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	holder := holdSessionsShareLock(t, ctx, pool)
	defer func() { _ = holder.Rollback(context.Background()) }()
	start := time.Now()
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(qctx, ddlSessionsTokenHash); err != nil {
		t.Fatalf("re-run behind a held share lock: %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("re-run waited %v, want no lock wait", waited)
	}
	requireTokenHashShape(t, ctx, pool)
}

func holdSessionsShareLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	if _, err := holder.Exec(ctx, "LOCK TABLE sage.sessions IN SHARE MODE"); err != nil {
		_ = holder.Rollback(context.Background())
		t.Fatalf("taking the share lock: %v", err)
	}
	return holder
}

func sessionsMigrationUser(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, email string,
) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ($1, 'x', 'viewer')
		ON CONFLICT (email) DO UPDATE SET role = 'viewer' RETURNING id`, email).
		Scan(&id); err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return id
}

func sessionRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, where, arg string,
) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.sessions WHERE "+where, arg).Scan(&n); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	return n
}

func requireTokenHashShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var dataType, nullable string
	if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns WHERE table_schema = 'sage'
		AND table_name = 'sessions' AND column_name = 'token_hash'`).
		Scan(&dataType, &nullable); err != nil {
		t.Fatalf("sage.sessions.token_hash missing: %v", err)
	}
	if dataType != "text" || nullable != "NO" {
		t.Fatalf("token_hash = %s nullable=%s, want text NOT NULL", dataType, nullable)
	}
	var unique bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'sage' AND indexname = 'idx_sessions_token_hash'
		AND indexdef LIKE 'CREATE UNIQUE INDEX%')`).Scan(&unique); err != nil {
		t.Fatalf("reading indexes: %v", err)
	}
	if !unique {
		t.Fatal("unique index idx_sessions_token_hash missing")
	}
}

func requireSQLState(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: err = %v, want SQLSTATE %s", what, err, code)
	}
}
