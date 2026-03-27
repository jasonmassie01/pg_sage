package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecConcurrently executes a SQL statement that requires
// CONCURRENTLY semantics (e.g., CREATE INDEX CONCURRENTLY).
// It acquires a raw connection from the pool, sets timeouts,
// executes outside a transaction, and returns the connection.
func ExecConcurrently(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql string,
	timeout time.Duration,
	opts ...DDLOption,
) error {
	o := applyDDLOpts(opts)

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

	if o.lockTimeoutMs > 0 {
		_, err = conn.Exec(ctx, fmt.Sprintf(
			"SET lock_timeout = '%dms'", o.lockTimeoutMs,
		))
		if err != nil {
			return fmt.Errorf("setting lock_timeout: %w", err)
		}
	}

	_, err = conn.Exec(ctx, sql)

	// Reset timeouts before returning connection to pool.
	_, _ = conn.Exec(ctx, "SET statement_timeout = 0")
	_, _ = conn.Exec(ctx, "SET lock_timeout = 0")

	if err != nil {
		return fmt.Errorf("executing DDL: %w", err)
	}

	return nil
}

// ExecInTransaction executes a SQL statement within a transaction
// for atomicity. Sets statement_timeout and lock_timeout.
func ExecInTransaction(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql string,
	timeout time.Duration,
	opts ...DDLOption,
) error {
	o := applyDDLOpts(opts)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	timeoutMs := int(timeout.Milliseconds())
	_, err = tx.Exec(ctx,
		fmt.Sprintf(
			"SET LOCAL statement_timeout = %d", timeoutMs,
		),
	)
	if err != nil {
		return fmt.Errorf("setting statement_timeout: %w", err)
	}

	if o.lockTimeoutMs > 0 {
		_, err = tx.Exec(ctx, fmt.Sprintf(
			"SET LOCAL lock_timeout = '%dms'",
			o.lockTimeoutMs,
		))
		if err != nil {
			return fmt.Errorf("setting lock_timeout: %w", err)
		}
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

// DDLOption configures DDL execution behavior.
type DDLOption func(*ddlOpts)

type ddlOpts struct {
	lockTimeoutMs int
}

func applyDDLOpts(opts []DDLOption) ddlOpts {
	var o ddlOpts
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithLockTimeout sets lock_timeout for DDL execution.
func WithLockTimeout(ms int) DDLOption {
	return func(o *ddlOpts) {
		o.lockTimeoutMs = ms
	}
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
