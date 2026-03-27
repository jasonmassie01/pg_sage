package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecConcurrently executes a SQL statement that requires CONCURRENTLY
// semantics (e.g., CREATE INDEX CONCURRENTLY). It acquires a raw connection
// from the pool, sets statement_timeout, executes outside a transaction,
// and returns the connection to the pool.
func ExecConcurrently(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql string,
	timeout time.Duration,
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Release()

	timeoutMs := int(timeout.Milliseconds())
	_, err = conn.Exec(ctx,
		fmt.Sprintf("SET statement_timeout = %d", timeoutMs),
	)
	if err != nil {
		return fmt.Errorf("setting statement_timeout: %w", err)
	}

	_, err = conn.Exec(ctx, sql)
	if err != nil {
		return fmt.Errorf("executing DDL: %w", err)
	}

	// Reset statement_timeout for the connection before returning to pool.
	_, _ = conn.Exec(ctx, "SET statement_timeout = 0")

	return nil
}

// ExecInTransaction executes a SQL statement within a transaction for
// atomicity. Sets statement_timeout within the transaction.
func ExecInTransaction(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql string,
	timeout time.Duration,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	timeoutMs := int(timeout.Milliseconds())
	_, err = tx.Exec(ctx,
		fmt.Sprintf("SET statement_timeout = %d", timeoutMs),
	)
	if err != nil {
		return fmt.Errorf("setting statement_timeout: %w", err)
	}

	_, err = tx.Exec(ctx, sql)
	if err != nil {
		return fmt.Errorf("executing DDL: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// NeedsConcurrently returns true if the SQL statement contains the
// CONCURRENTLY keyword, indicating it cannot run inside a transaction.
func NeedsConcurrently(sql string) bool {
	return strings.Contains(strings.ToUpper(sql), "CONCURRENTLY")
}

// NeedsTopLevel returns true if the SQL statement cannot run inside a
// transaction block. VACUUM is the primary example: PostgreSQL raises
// "VACUUM cannot be executed from a function or multi-command string"
// when attempted inside a transaction.
func NeedsTopLevel(sql string) bool {
	upper := strings.TrimSpace(strings.ToUpper(sql))
	return strings.HasPrefix(upper, "VACUUM")
}
