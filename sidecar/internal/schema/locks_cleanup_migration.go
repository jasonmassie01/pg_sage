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
// its lock in time, or runs out of its time budget, stops without failing
// the bootstrap and resumes on the next one.

// locksCleanupName is the cleanup's sage.data_cleanup row.
const locksCleanupName = "collection-01 locks raw sql"

// locksCleanupBatch rows are deleted per transaction.
const locksCleanupBatch = 10000

// locksCleanupBudget bounds one bootstrap's cleanup (startup bootstraps
// under a short deadline); the rest waits for the next bootstrap.
var locksCleanupBudget = 5 * time.Second

const ddlDataCleanup = `
CREATE TABLE IF NOT EXISTS sage.data_cleanup (
    name         text PRIMARY KEY,
    cutoff       timestamptz NOT NULL,
    completed_at timestamptz
)`

// locksCleanupDelete deletes one batch older than the cutoff ($1). The
// table may be day-partitioned, so a row is named by (collected_at, id).
var locksCleanupDelete = `DELETE FROM sage.snapshots
 WHERE (collected_at, id) IN (
       SELECT collected_at, id FROM sage.snapshots
        WHERE category = 'locks' AND collected_at < $1
        LIMIT ` + strconv.Itoa(locksCleanupBatch) + `)`

// migrateLocksCleanup deletes the pre-wait-graph locks rows (see above).
func migrateLocksCleanup(ctx context.Context, db bootstrapDB) error {
	cutoff, done, err := locksCleanupMarker(ctx, db)
	if err != nil || done {
		return err
	}
	deadline := time.Now().Add(locksCleanupBudget)
	for {
		deleted, err := deleteLocksBatch(ctx, db, cutoff)
		if locksCleanupDeferrable(err) {
			slog.Warn("schema: locks history cleanup deferred to the next bootstrap",
				"error", err)
			return nil
		}
		if err != nil {
			return fmt.Errorf("delete pre-wait-graph locks snapshots: %w", err)
		}
		if deleted < locksCleanupBatch {
			break
		}
		if !time.Now().Before(deadline) {
			slog.Info("schema: locks history cleanup paused (time budget); "+
				"the next bootstrap resumes it", "budget", locksCleanupBudget)
			return nil
		}
	}
	if _, err := db.Exec(ctx, `UPDATE sage.data_cleanup SET completed_at = now()
		WHERE name = $1`, locksCleanupName); err != nil {
		return fmt.Errorf("mark the locks cleanup complete: %w", err)
	}
	return nil
}

// locksCleanupMarker records the cleanup's cutoff on its first run and
// returns it, and whether the cleanup already finished.
func locksCleanupMarker(ctx context.Context, db bootstrapDB) (time.Time, bool, error) {
	if _, err := db.Exec(ctx, ddlDataCleanup); err != nil {
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

// deleteLocksBatch deletes one batch in its own transaction under
// lock_timeout and statement_timeout, and returns how many rows it deleted.
func deleteLocksBatch(ctx context.Context, db bootstrapDB, cutoff time.Time) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1s';
		SET LOCAL statement_timeout = '10s'`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, locksCleanupDelete, cutoff)
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
