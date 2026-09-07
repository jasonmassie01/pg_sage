package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLockNotAvailable is returned when a DDL operation fails because
// PostgreSQL could not acquire the required lock within lock_timeout.
// Callers should circuit-break the table and avoid immediate retry.
var ErrLockNotAvailable = errors.New("lock not available")

// IsLockNotAvailable returns true if the error is a PostgreSQL
// lock_not_available error (SQLSTATE 55P03). This occurs when
// lock_timeout expires before the required lock can be acquired.
func IsLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

// wrapDDLError wraps a DDL execution error. If the underlying cause
// is a lock_not_available error, it wraps with ErrLockNotAvailable
// so callers can match with errors.Is.
func wrapDDLError(err error) error {
	if IsLockNotAvailable(err) {
		return fmt.Errorf("%w: %w", ErrLockNotAvailable, err)
	}
	return fmt.Errorf("executing DDL: %w", err)
}

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
	if err := ValidateExecutorSQL(sql); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}

	o := applyDDLOpts(opts)

	return withTopLevelSession(ctx, pool, timeout, o, func(conn pooledExecutor) error {
		_, execErr := conn.Exec(ctx, sql)
		if execErr != nil {
			return wrapDDLError(execErr)
		}
		return nil
	})
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
	if err := ValidateExecutorSQL(sql); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}

	o := applyDDLOpts(opts)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

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
		return wrapDDLError(err)
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
// transaction block. VACUUM and ALTER SYSTEM both raise errors when
// attempted inside a transaction.
func NeedsTopLevel(sql string) bool {
	upper := strings.TrimSpace(strings.ToUpper(sql))
	return strings.HasPrefix(upper, "VACUUM") ||
		strings.HasPrefix(upper, "ALTER SYSTEM")
}
