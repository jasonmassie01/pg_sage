package schema

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Bootstrap's session settings (config-04): every bootstrap statement waits
// at most migrationLockTimeout for a lock. The setting is reset before the
// advisory lock is released; a session whose reset fails is closed, not
// returned to the pool with it.
var (
	bootstrapSetLockTimeoutSQL = fmt.Sprintf("SET lock_timeout = '%dms'",
		migrationLockTimeout.Milliseconds())
	bootstrapResetLockTimeoutSQL = "RESET lock_timeout"
)

// withBootstrapSession runs fn on the advisory-locked bootstrap session with
// lock_timeout set, then resets it and releases the lock, in that order.
func withBootstrapSession(
	ctx context.Context, pool *pgxpool.Pool, fn func(*pgxpool.Conn) error,
) (returnErr error) {
	lock, err := acquireAdvisoryLock(ctx, pool, bootstrapLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, lock.Release(ctx))
	}()
	if _, err := lock.conn.Exec(ctx, bootstrapSetLockTimeoutSQL); err != nil {
		lock.discard = true
		return fmt.Errorf("schema bootstrap: set lock_timeout: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), bootstrapUnlockTimeout,
		)
		defer cancel()
		if _, err := lock.conn.Exec(cleanupCtx, bootstrapResetLockTimeoutSQL); err != nil {
			lock.discard = true
			returnErr = errors.Join(returnErr, fmt.Errorf(
				"schema bootstrap: reset lock_timeout (session closed instead): %w", err))
		}
	}()
	return fn(lock.conn)
}
