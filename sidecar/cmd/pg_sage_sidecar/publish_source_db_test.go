package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// pub-01 review: the publish snapshot is read-only, repeatable read and
// bounded by statement_timeout and lock_timeout, so publishing can never
// write to a monitored database or queue behind a lock for long.
func TestPublishSource_DBSnapshotIsReadOnlyAndBounded(t *testing.T) {
	_, dsn, _ := publishDB(t)
	ctx := context.Background()
	src, err := openPublishSource(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer src.Close()
	tx := src.(*pgPublishSource).tx
	var readOnly, isolation, stmt, lock string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only'),
		current_setting('transaction_isolation'), current_setting('statement_timeout'),
		current_setting('lock_timeout')`).Scan(&readOnly, &isolation, &stmt,
		&lock); err != nil {
		t.Fatalf("read the snapshot's settings: %v", err)
	}
	if readOnly != "on" || isolation != "repeatable read" || stmt != "1min" ||
		lock != "5s" {
		t.Fatalf("snapshot read_only=%s isolation=%s statement_timeout=%s "+
			"lock_timeout=%s; want on, repeatable read, 1min, 5s", readOnly, isolation,
			stmt, lock)
	}
	_, err = tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome) VALUES ('analyze_table', 'ANALYZE t', 'success')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("a write through the snapshot = %v, want read_only_sql_transaction", err)
	}
}
