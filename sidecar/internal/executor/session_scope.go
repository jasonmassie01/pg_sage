package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pooledExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func withTopLevelSession(
	ctx context.Context,
	pool *pgxpool.Pool,
	timeout time.Duration,
	opts ddlOpts,
	run func(pooledExecutor) error,
) (retErr error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer func() {
		if cleanupErr := resetTopLevelSession(conn); cleanupErr != nil {
			discardPooledSession(conn)
			retErr = errors.Join(retErr, cleanupErr)
			return
		}
		conn.Release()
	}()
	if err := setTopLevelTimeouts(ctx, conn, timeout, opts); err != nil {
		return err
	}
	return run(conn)
}

func setTopLevelTimeouts(
	ctx context.Context,
	conn pooledExecutor,
	timeout time.Duration,
	opts ddlOpts,
) error {
	timeoutMs := timeout.Milliseconds()
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		"SET statement_timeout = %d", timeoutMs,
	)); err != nil {
		return fmt.Errorf("setting statement_timeout: %w", err)
	}
	if opts.lockTimeoutMs == 0 {
		return nil
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		"SET lock_timeout = '%dms'", opts.lockTimeoutMs,
	)); err != nil {
		return fmt.Errorf("setting lock_timeout: %w", err)
	}
	return nil
}

func resetTopLevelSession(conn pooledExecutor) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.Exec(cleanupCtx, "RESET statement_timeout"); err != nil {
		return fmt.Errorf("resetting statement_timeout: %w", err)
	}
	if _, err := conn.Exec(cleanupCtx, "RESET lock_timeout"); err != nil {
		return fmt.Errorf("resetting lock_timeout: %w", err)
	}
	return nil
}

func discardPooledSession(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = raw.Close(cleanupCtx)
}
