package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

type pooledExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// executorSearchPath is the search_path pg_sage's own DDL runs under. Only
// pg_catalog and the session's temporary schema (searched last) resolve
// unqualified names, so a function or operator another role planted in
// public can never bind into pg_sage's DDL (the CVE-2018-1058 class).
const executorSearchPath = "pg_catalog, pg_temp"

// topLevelSessionSettings are the settings a top-level session changes and
// resets, in order, before its connection returns to the pool.
var topLevelSessionSettings = []string{"statement_timeout", "lock_timeout", "search_path"}

// topLevelSession is one pooled connection an action runs on outside a
// transaction: released when its settings reset cleanly, discarded if not.
type topLevelSession interface {
	pooledExecutor
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Release()
	discard()
}

// readSearchPathSQL reads the session's search_path before an action pins
// it; restoreSearchPathSQL puts exactly that value back afterwards. A
// RESET would restore the role's path instead, and a session of a pg_sage
// pool (pinned to pg_catalog, pg_temp at connect, security-01c-r) would go
// back to the pool unpinned.
const (
	readSearchPathSQL    = "SELECT pg_catalog.current_setting('search_path')"
	restoreSearchPathSQL = "SELECT pg_catalog.set_config('search_path', $1, false)"
)

// pooledSession is a pool connection and the statement it will run, which
// an emergency stop cancels while it runs.
type pooledSession struct {
	*pgxpool.Conn
	sql string
}

func (s pooledSession) discard() { discardPooledSession(s.Conn) }

// watchSession registers a pooled session's statement with the run's
// emergency stop. Any other session has no server connection to cancel.
func watchSession(ctx context.Context, sess topLevelSession) (func(), error) {
	ps, ok := sess.(pooledSession)
	if !ok {
		return func() {}, nil
	}
	return watchStatement(ctx, ps.Conn.Conn().PgConn(), ps.sql)
}

func withTopLevelSession(
	ctx context.Context,
	pool *pgxpool.Pool,
	sql string,
	timeout time.Duration,
	opts ddlOpts,
	run func(pooledExecutor) error,
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	return runTopLevelSession(ctx, pooledSession{Conn: conn, sql: sql}, timeout, opts, run)
}

// runTopLevelSession scopes the session for one action, runs it, and
// returns the connection to the pool only when every setting was reset: a
// connection still pinned or still timed out is closed instead.
func runTopLevelSession(
	ctx context.Context,
	sess topLevelSession,
	timeout time.Duration,
	opts ddlOpts,
	run func(pooledExecutor) error,
) (retErr error) {
	var prior string
	if err := sess.QueryRow(ctx, readSearchPathSQL).Scan(&prior); err != nil {
		sess.discard()
		return fmt.Errorf("reading search_path: %w", err)
	}
	defer func() {
		if cleanupErr := resetTopLevelSession(sess, opts, prior); cleanupErr != nil {
			sess.discard()
			retErr = errors.Join(retErr, cleanupErr)
			return
		}
		sess.Release()
	}()
	// Deferred after the cleanup, so it runs first: the session is reset
	// only once a stop's cancel request has been delivered.
	unwatch, err := watchSession(ctx, sess)
	if err != nil {
		return err
	}
	defer unwatch()
	if err := setTopLevelSession(ctx, sess, timeout, opts); err != nil {
		return err
	}
	return run(sess)
}

// setTopLevelSession sets the session's statement and lock timeouts and
// pins its search_path; SET LOCAL is unavailable outside a transaction.
func setTopLevelSession(
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
	if opts.lockTimeoutMs != 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			"SET lock_timeout = '%dms'", opts.lockTimeoutMs,
		)); err != nil {
			return fmt.Errorf("setting lock_timeout: %w", err)
		}
	}
	if _, err := conn.Exec(ctx, "SET search_path = "+executorSearchPath); err != nil {
		return fmt.Errorf("setting search_path: %w", err)
	}
	return setActionTag(ctx, conn, "SET", opts)
}

// setActionTag names the action's decision in application_name; verb is
// SET or SET LOCAL. The name keeps pg_sage in it, so every collector still
// excludes the session.
func setActionTag(ctx context.Context, conn pooledExecutor, verb string, o ddlOpts) error {
	if o.actionTag <= 0 {
		return nil
	}
	name := selfmonitor.ActionApplicationName(o.actionTag)
	if _, err := conn.Exec(ctx, verb+" application_name = '"+name+"'"); err != nil {
		return fmt.Errorf("setting application_name: %w", err)
	}
	return nil
}

// resetTopLevelSession resets every session setting (search_path to path,
// the session's own value before the action), and the action tag when one
// was set; the first failure stops it, and the caller then discards the
// connection.
func resetTopLevelSession(conn pooledExecutor, o ddlOpts, path string) error {
	settings := topLevelSessionSettings
	if o.actionTag > 0 {
		settings = append(slices.Clone(settings), "application_name")
	}
	for _, setting := range settings {
		if err := resetSessionSetting(conn, setting, path); err != nil {
			return err
		}
	}
	return nil
}

// resetSessionSetting gives each reset its own cleanup budget, so a slow
// round trip on one cannot starve the next.
func resetSessionSetting(conn pooledExecutor, setting, path string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sql, args := "RESET "+setting, []any(nil)
	if setting == "search_path" {
		sql, args = restoreSearchPathSQL, []any{path}
	}
	if _, err := conn.Exec(cleanupCtx, sql, args...); err != nil {
		return fmt.Errorf("resetting %s: %w", setting, err)
	}
	return nil
}

func discardPooledSession(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = raw.Close(cleanupCtx)
}
