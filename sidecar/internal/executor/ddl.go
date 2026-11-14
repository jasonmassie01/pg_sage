package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
// It acquires a raw connection from the pool, sets timeouts, pins
// search_path (pg_catalog, pg_temp), executes outside a transaction,
// and returns the connection with every setting reset. A bare extension
// operator class in a CREATE INDEX is run with its schema named
// (qualifyIndexOpclasses).
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
	run, err := pinnedStatements(ctx, pool, sql)
	if err != nil {
		return stoppedStatement(ctx, sql, err)
	}

	o := applyDDLOpts(opts)

	err = withTopLevelSession(ctx, pool, run[0], timeout, o, func(conn pooledExecutor) error {
		reportBackendPID(conn, o)
		_, execErr := conn.Exec(ctx, run[0])
		if execErr != nil {
			return wrapDDLError(execErr)
		}
		return nil
	})
	return stoppedStatement(ctx, run[0], err)
}

// ExecInTransaction executes a SQL statement within a transaction
// for atomicity. Sets statement_timeout and lock_timeout and pins
// search_path (pg_catalog, pg_temp), so targets must be schema-qualified.
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
	return execStatementsInTransaction(ctx, pool, timeout, applyDDLOpts(opts), sql)
}

// execStatementsInTransaction runs already validated statements in one
// transaction under statement_timeout and lock_timeout, with search_path
// pinned to pg_catalog: all or none. A bare extension operator class in a
// CREATE INDEX runs with its schema named. An emergency stop cancels them
// while they run.
func execStatementsInTransaction(
	ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, o ddlOpts,
	statements ...string,
) error {
	run, err := pinnedStatements(ctx, pool, statements...)
	if err == nil {
		err = execTransaction(ctx, pool, timeout, o, run)
	}
	return stoppedStatement(ctx, strings.Join(statements, "; "), err)
}

func execTransaction(
	ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, o ddlOpts,
	statements []string,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	// Deferred after the rollback, so it runs first (see runTopLevelSession).
	unwatch, err := watchStatement(ctx, tx.Conn().PgConn(), strings.Join(statements, "; "))
	if err != nil {
		return err
	}
	defer unwatch()
	if err := setLocalSessionScope(ctx, tx, timeout, o); err != nil {
		return err
	}
	if o.before != nil {
		if err := o.before(ctx, tx); err != nil {
			return err
		}
	}
	for _, sql := range statements {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return wrapDDLError(err)
		}
	}

	if o.after != nil {
		if err := o.after(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// setLocalSessionScope sets the transaction's statement and lock timeouts
// and pins its search_path (and names its decision); every setting ends
// with the transaction.
func setLocalSessionScope(
	ctx context.Context, tx pooledExecutor, timeout time.Duration, o ddlOpts,
) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"SET LOCAL statement_timeout = %d", timeout.Milliseconds(),
	)); err != nil {
		return fmt.Errorf("setting statement_timeout: %w", err)
	}
	if o.lockTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"SET LOCAL lock_timeout = '%dms'", o.lockTimeoutMs,
		)); err != nil {
			return fmt.Errorf("setting lock_timeout: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+executorSearchPath); err != nil {
		return fmt.Errorf("setting search_path: %w", err)
	}
	return setActionTag(ctx, tx, "SET LOCAL", o)
}

// DDLOption configures DDL execution behavior.
type DDLOption func(*ddlOpts)

type ddlOpts struct {
	lockTimeoutMs int
	before        func(context.Context, pgx.Tx) error
	after         func(context.Context, pgx.Tx) error
	// onBackendPID receives the server process of a top-level statement
	// just before it runs (the remnant tracker watches that build).
	onBackendPID func(uint32)
	// actionTag is the decision the statement runs for (0: none); its
	// session's application_name names it (audit-06).
	actionTag int64
	// resetCostDelay resets vacuum_cost_delay with the session's other
	// settings: an emergency VACUUM sets it to 0 (vacuum-05b).
	resetCostDelay bool
}

func applyDDLOpts(opts []DDLOption) ddlOpts {
	var o ddlOpts
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// withBackendPID reports the backend a top-level statement runs on.
func withBackendPID(fn func(uint32)) DDLOption {
	return func(o *ddlOpts) {
		o.onBackendPID = fn
	}
}

func reportBackendPID(conn pooledExecutor, o ddlOpts) {
	if o.onBackendPID == nil {
		return
	}
	if ps, ok := conn.(pooledSession); ok {
		o.onBackendPID(ps.Conn.Conn().PgConn().PID())
	}
}

// WithLockTimeout sets lock_timeout for DDL execution.
func WithLockTimeout(ms int) DDLOption {
	return func(o *ddlOpts) {
		o.lockTimeoutMs = ms
	}
}

// WithActionTag names decisionID in the session's application_name
// ('pg_sage action:<id>') while the statement runs, so its pgaudit records
// are tied to the decision. A decision id of 0 or below tags nothing.
func WithActionTag(decisionID int64) DDLOption {
	return func(o *ddlOpts) {
		if decisionID > 0 {
			o.actionTag = decisionID
		}
	}
}

// execMode is how a statement runs: inside a transaction block (under the
// policy lock ceiling) or top-level, outside one.
type execMode int

const (
	execInTransaction execMode = iota
	execTopLevel
)

// executionMode routes a statement by its typed action (executor-08), never
// by a keyword anywhere in its text: only the CONCURRENTLY index builds and
// drops, a one-table VACUUM and ALTER SYSTEM cannot run in a transaction
// block. Every other type, and an unclassified statement, runs in one.
func executionMode(actionType string) execMode {
	switch actionType {
	case "create_index_concurrently", "reindex_concurrently", "drop_unused_index",
		"revert_created_index", "vacuum_table", "alter_system_guc":
		return execTopLevel
	}
	return execInTransaction
}

// statementRunsTopLevel is the route of sql's typed action.
func statementRunsTopLevel(sql string) bool {
	return executionMode(ActionTypeForSQL(sql)) == execTopLevel
}
