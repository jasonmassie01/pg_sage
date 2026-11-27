package schema

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Locks history cleanup (collection-01). Before the bounded wait graph,
// every locks row held every lock of every session with its raw statement
// text, literals included, kept for the snapshot retention and served by
// /snapshots/latest. Nothing reads lock history (/snapshots/history
// refuses the category), so those rows are deleted once, in batches under
// lock_timeout, in every database Bootstrap runs on (monitored and meta).
//
// sage.data_cleanup records the cleanup: its cutoff (when it first ran) and
// when it finished. Only rows collected before the cutoff are deleted, so
// the new collector's rows survive an interrupted cleanup resumed by a
// later bootstrap; a finished cleanup is a no-op. A cleanup that cannot get
// its lock in time, or runs out of its time budget or of its caller's
// deadline, stops without failing the bootstrap and resumes on the next
// one; each batch's statement_timeout stays inside both.

// locksCleanupName is the cleanup's sage.data_cleanup row.
const locksCleanupName = "collection-01 locks raw sql"

// locksCleanupBatch rows are deleted per transaction at first; a batch cut
// by its statement_timeout is retried at a tenth of its size, down to
// locksCleanupMinBatch, so very large legacy rows still make progress.
const (
	locksCleanupBatch    = 10000
	locksCleanupMinBatch = 10
)

// locksCleanupBudget bounds one bootstrap's cleanup (startup bootstraps
// under a short deadline); the rest waits for the next bootstrap.
var locksCleanupBudget = 5 * time.Second

// A batch's statement_timeout is half the time left, clamped to these, and
// always under half of what is left of the caller's context, so the server
// cuts a slow batch before the caller's deadline fails the bootstrap.
const (
	locksCleanupMinSlice = 500 * time.Millisecond
	locksCleanupMaxSlice = 10 * time.Second
	// locksCleanupNoTime is too short a timeout to start a batch with
	// (statement_timeout 0 would disable the timeout).
	locksCleanupNoTime = 10 * time.Millisecond
)

const ddlDataCleanup = `
CREATE TABLE IF NOT EXISTS sage.data_cleanup (
    name         text PRIMARY KEY,
    cutoff       timestamptz NOT NULL,
    completed_at timestamptz
)`

// locksCleanupDelete deletes one batch ($2 rows) older than the cutoff
// ($1). The table may be day-partitioned, so a row is named by
// (collected_at, id).
const locksCleanupDelete = `DELETE FROM sage.snapshots
 WHERE (collected_at, id) IN (
       SELECT collected_at, id FROM sage.snapshots
        WHERE category = 'locks' AND collected_at < $1
        LIMIT $2)`

// migrateLocksCleanup deletes the pre-wait-graph locks rows (see above).
func migrateLocksCleanup(ctx context.Context, db bootstrapDB) error {
	cutoff, done, err := locksCleanupMarker(ctx, db)
	if err != nil || done {
		return err
	}
	deadline := locksCleanupDeadline(ctx)
	batch := locksCleanupBatch
	for {
		slice := locksBatchTimeout(ctx, deadline)
		if slice < locksCleanupNoTime {
			slog.Info("schema: locks history cleanup paused (no time left); " +
				"the next bootstrap resumes it")
			return nil
		}
		deleted, err := deleteLocksBatch(ctx, db, cutoff, batch, slice)
		switch {
		case isStatementTimeout(err) && batch > locksCleanupMinBatch:
			// Retried past the budget too (at most three more batches
			// at the floor timeout), so every run tries a small batch.
			batch = max(batch/10, locksCleanupMinBatch)
			continue
		case locksCleanupDeferrable(err):
			slog.Warn("schema: locks history cleanup deferred to the next bootstrap",
				"error", err)
			return nil
		case err != nil:
			return fmt.Errorf("delete pre-wait-graph locks snapshots: %w", err)
		case deleted < int64(batch):
			return markLocksCleanupDone(ctx, db)
		case !time.Now().Before(deadline):
			slog.Info("schema: locks history cleanup paused (time budget); "+
				"the next bootstrap resumes it", "budget", locksCleanupBudget)
			return nil
		}
	}
}

// locksCleanupDeadline ends the cleanup after locksCleanupBudget, or
// halfway to ctx's deadline when that comes first, leaving the rest of the
// caller's time to the steps after it.
func locksCleanupDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(locksCleanupBudget)
	if cd, ok := ctx.Deadline(); ok {
		if half := time.Now().Add(time.Until(cd) / 2); half.Before(deadline) {
			deadline = half
		}
	}
	return deadline
}

// locksBatchTimeout is the next batch's statement_timeout: half the time
// left to deadline, clamped to [locksCleanupMinSlice, locksCleanupMaxSlice],
// and at most half of what is left of ctx's deadline.
func locksBatchTimeout(ctx context.Context, deadline time.Time) time.Duration {
	slice := min(max(time.Until(deadline)/2, locksCleanupMinSlice), locksCleanupMaxSlice)
	if cd, ok := ctx.Deadline(); ok {
		slice = min(slice, time.Until(cd)/2)
	}
	return slice
}

// markLocksCleanupDone records that the cleanup finished: later bootstraps
// skip it.
func markLocksCleanupDone(ctx context.Context, db bootstrapDB) error {
	if _, err := db.Exec(ctx, `UPDATE sage.data_cleanup SET completed_at = now()
		WHERE name = $1`, locksCleanupName); err != nil {
		return fmt.Errorf("mark the locks cleanup complete: %w", err)
	}
	return nil
}

// locksCleanupMarker records the cleanup's cutoff on its first run and
// returns it, and whether the cleanup already finished.
func locksCleanupMarker(ctx context.Context, db bootstrapDB) (time.Time, bool, error) {
	if _, err := runMigrationStep(ctx, db, migrationStep{
		name: "ddlDataCleanup", sql: ddlDataCleanup,
	}); err != nil {
		return time.Time{}, false, fmt.Errorf("create sage.data_cleanup: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO sage.data_cleanup (name, cutoff)
		VALUES ($1, now()) ON CONFLICT (name) DO NOTHING`, locksCleanupName); err != nil {
		return time.Time{}, false, fmt.Errorf("record the locks cleanup cutoff: %w", err)
	}
	var cutoff time.Time
	var completed *time.Time
	if err := db.QueryRow(ctx, `SELECT cutoff, completed_at FROM sage.data_cleanup
		WHERE name = $1`, locksCleanupName).Scan(&cutoff, &completed); err != nil {
		return time.Time{}, false, fmt.Errorf("read the locks cleanup marker: %w", err)
	}
	return cutoff, completed != nil, nil
}

// deleteLocksBatch deletes one batch of at most batch rows in its own
// transaction, with lock_timeout and statement_timeout at most timeout, and
// returns how many rows it deleted.
func deleteLocksBatch(ctx context.Context, db bootstrapDB, cutoff time.Time,
	batch int, timeout time.Duration) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ms := strconv.FormatInt(timeout.Milliseconds(), 10) + "ms"
	lockMS := strconv.FormatInt(min(timeout, time.Second).Milliseconds(), 10) + "ms"
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true),
		set_config('statement_timeout', $2, true)`, lockMS, ms); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, locksCleanupDelete, cutoff, batch)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// locksCleanupDeferrable reports a lock or statement timeout: the cleanup
// waits for the next bootstrap instead of failing this one.
func locksCleanupDeferrable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "55P03" || pgErr.Code == "57014"
}

// isStatementTimeout reports a statement cut by statement_timeout.
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}
