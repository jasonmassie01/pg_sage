package workpack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// evid-02a: a build's read of the history (which may be the monitored
// database) is read-only and each statement is bounded. No concurrency
// test: each build opens its own transaction.

func TestBeginRead_IsReadOnlyAndBounded(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "workpack_readonly")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := context.Background()
	tx, err := beginRead(ctx, histstore.Resolve(pool))
	if err != nil {
		t.Fatalf("beginRead: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ro, timeout string
	if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&ro); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if ro != "on" || timeout != readTimeout {
		t.Fatalf("read transaction: read_only %q, statement_timeout %q; want on and %s",
			ro, timeout, readTimeout)
	}
	_, err = tx.Exec(ctx, "CREATE TABLE workpack_must_not_exist (id int)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("a write in the read transaction: %v, want SQLSTATE 25006 (read-only)", err)
	}
}

func TestBeginRead_ACanceledContextNamesTheRead(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "workpack_readcancel")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := beginRead(ctx, histstore.Resolve(pool))
	if tx != nil || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "open a read of the history") {
		t.Fatalf("beginRead on a canceled context: tx %v err %v, want context.Canceled", tx,
			err)
	}
}
