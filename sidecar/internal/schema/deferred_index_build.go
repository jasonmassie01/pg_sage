package schema

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The background builder (config-04c). It runs on a dedicated session (a
// CREATE INDEX CONCURRENTLY cannot run in a transaction block) under a
// session advisory lock distinct from the bootstrap lock, so one sidecar
// per database builds, and never beside a boot's own lock. Each build
// waits at most lock_timeout for a lock (a failure is retried later) and
// has no statement_timeout; maintenance_work_mem is the server's. One
// index at a time. Cancelling ctx cancels the running statement on the
// server and leaves an INVALID index, which the next run drops and builds
// again.

// ErrDeferredBuildBusy reports that another session holds the builder's
// lock: another sidecar is building this database's deferred indexes.
var ErrDeferredBuildBusy = errors.New(
	"another sidecar is building the deferred sage indexes of this database")

// deferredBuildLockKey names the builder's advisory lock (hashtext), apart
// from bootstrapAdvisoryLockKey.
const deferredBuildLockKey = "pg_sage deferred index builder"

const (
	// buildCancelTimeout bounds the cancel request sent when ctx ends.
	buildCancelTimeout = 5 * time.Second
	// buildCancelGrace is how long a cancelled build may take to stop
	// before the session is closed instead.
	buildCancelGrace  = 10 * time.Second
	buildCloseTimeout = 5 * time.Second
)

// BuildReport is what one BuildDeferredIndexes run did, by index name.
type BuildReport struct {
	Built    []string // created VALID by this run
	Skipped  []string // their table does not exist
	Building []string // INVALID with a build in progress elsewhere
	Failed   []string // the drop or the build failed
}

// DeferredIndexCounts is how many declared deferred indexes need a build,
// by state (the pg_sage_schema_deferred_indexes gauge).
type DeferredIndexCounts struct {
	Missing  int // absent while its table exists
	Building int // INVALID with a build in progress
	Invalid  int // INVALID with no build in progress
}

func (c DeferredIndexCounts) pending() int { return c.Missing + c.Building + c.Invalid }

// deferredBuildState is one deferred index's catalog state.
type deferredBuildState struct {
	tableExists, exists, valid, inProgress bool
}

const deferredBuildStateSQL = `SELECT to_regclass('sage.' || $1) IS NOT NULL,
       to_regclass('sage.' || $2) IS NOT NULL,
       COALESCE((SELECT i.indisvalid FROM pg_catalog.pg_index i
                  WHERE i.indexrelid = to_regclass('sage.' || $2)), false),
       EXISTS (SELECT 1 FROM pg_catalog.pg_stat_progress_create_index p
                WHERE p.index_relid = to_regclass('sage.' || $2)
                  AND p.datid = (SELECT oid FROM pg_catalog.pg_database
                                  WHERE datname = current_database()))`

type buildStateQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readBuildState(ctx context.Context, db buildStateQuerier, idx ledgerIndex) (
	deferredBuildState, error) {
	var s deferredBuildState
	err := db.QueryRow(ctx, deferredBuildStateSQL, idx.table, idx.name).
		Scan(&s.tableExists, &s.exists, &s.valid, &s.inProgress)
	if err != nil {
		return s, fmt.Errorf("reading the state of deferred index sage.%s: %w", idx.name, err)
	}
	return s, nil
}

// ReadDeferredIndexStates counts the declared deferred indexes that need
// a build. An index whose table does not exist is not counted.
func ReadDeferredIndexStates(ctx context.Context, pool *pgxpool.Pool) (
	DeferredIndexCounts, error) {
	var c DeferredIndexCounts
	if len(deferredIndexes) == 0 || pool == nil {
		return c, nil
	}
	for _, idx := range deferredIndexes {
		s, err := readBuildState(ctx, pool, idx)
		if err != nil {
			return c, err
		}
		switch {
		case !s.tableExists || s.valid:
		case !s.exists:
			c.Missing++
		case s.inProgress:
			c.Building++
		default:
			c.Invalid++
		}
	}
	return c, nil
}

// BuildDeferredIndexes builds every declared deferred index that is absent
// or INVALID, CONCURRENTLY, one at a time. It returns ErrDeferredBuildBusy
// when another session holds the builder's lock.
func BuildDeferredIndexes(ctx context.Context, pool *pgxpool.Pool) (BuildReport, error) {
	var report BuildReport
	if len(deferredIndexes) == 0 {
		return report, nil
	}
	if pool == nil {
		return report, errors.New("deferred index build requires a PostgreSQL connection pool")
	}
	conn, err := openBuildSession(ctx, pool)
	if err != nil {
		return report, err
	}
	defer closeBuildSession(conn)
	var errs []error
	for _, idx := range deferredIndexes {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := buildDeferredIndex(ctx, conn, idx, &report); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

// openBuildSession takes a session out of the pool for good (it is closed
// afterwards, so no setting returns to the pool), takes the builder's
// lock and sets the build's timeouts.
func openBuildSession(ctx context.Context, pool *pgxpool.Pool) (*pgx.Conn, error) {
	pooled, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("deferred index build: acquiring a session: %w", err)
	}
	conn := pooled.Hijack()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))",
		deferredBuildLockKey).Scan(&locked); err != nil {
		closeBuildSession(conn)
		return nil, fmt.Errorf("deferred index build: taking the builder lock: %w", err)
	}
	if !locked {
		closeBuildSession(conn)
		return nil, ErrDeferredBuildBusy
	}
	settings := fmt.Sprintf("SET lock_timeout = '%dms'; SET statement_timeout = 0",
		migrationLockTimeout.Milliseconds())
	if _, err := conn.Exec(ctx, settings); err != nil {
		closeBuildSession(conn)
		return nil, fmt.Errorf("deferred index build: setting lock_timeout: %w", err)
	}
	return conn, nil
}

// closeBuildSession ends the session, which releases the builder's lock.
func closeBuildSession(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), buildCloseTimeout)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		slog.Debug("schema: closing the deferred index build session", "error", err)
	}
}

// buildDeferredIndex drops an INVALID copy no build is working on, then
// creates the index CONCURRENTLY.
func buildDeferredIndex(ctx context.Context, conn *pgx.Conn, idx ledgerIndex,
	report *BuildReport) error {
	s, err := readBuildState(ctx, conn, idx)
	if err != nil {
		return err
	}
	switch {
	case !s.tableExists:
		slog.Info("schema: deferred index skipped: its table does not exist",
			"index", "sage."+idx.name, "table", "sage."+idx.table)
		report.Skipped = append(report.Skipped, idx.name)
		return nil
	case s.exists && s.valid:
		return nil
	case s.exists && s.inProgress:
		report.Building = append(report.Building, idx.name)
		return nil
	case s.exists:
		drop := "DROP INDEX CONCURRENTLY IF EXISTS sage." + pgx.Identifier{idx.name}.Sanitize()
		if err := execCancellable(ctx, conn, drop); err != nil {
			report.Failed = append(report.Failed, idx.name)
			return fmt.Errorf("dropping INVALID deferred index sage.%s on sage.%s: %w",
				idx.name, idx.table, err)
		}
	}
	start := time.Now()
	if err := execCancellable(ctx, conn, concurrentIndexSQL(idx)); err != nil {
		report.Failed = append(report.Failed, idx.name)
		return fmt.Errorf("building deferred index sage.%s on sage.%s: %w",
			idx.name, idx.table, err)
	}
	slog.Info("schema: deferred index built", "index", "sage."+idx.name,
		"table", "sage."+idx.table, "ms", time.Since(start).Milliseconds())
	report.Built = append(report.Built, idx.name)
	return nil
}

// concurrentIndexSQL is the declared definition as CREATE [UNIQUE] INDEX
// CONCURRENTLY IF NOT EXISTS.
func concurrentIndexSQL(idx ledgerIndex) string {
	def := strings.Replace(idx.definition, "INDEX %I",
		"INDEX CONCURRENTLY IF NOT EXISTS %I", 1)
	return "CREATE " + strings.Replace(def, "%I", pgx.Identifier{idx.name}.Sanitize(), 1)
}

// execCancellable runs sql so that cancelling ctx cancels it on the server
// (a cancel request) and the session waits for the server to stop it, so
// the build is over before the session closes and releases the builder's
// lock. If the server does not stop within buildCancelGrace, the client
// gives up on the statement (pgx then closes the session).
func execCancellable(ctx context.Context, conn *pgx.Conn, sql string) error {
	execCtx, cancelExec := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelExec()
	stop := context.AfterFunc(ctx, func() {
		cctx, cancel := context.WithTimeout(context.Background(), buildCancelTimeout)
		defer cancel()
		if err := conn.PgConn().CancelRequest(cctx); err != nil {
			cancelExec()
			return
		}
		time.AfterFunc(buildCancelGrace, cancelExec)
	})
	defer stop()
	_, err := conn.Exec(execCtx, sql)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return err
}
