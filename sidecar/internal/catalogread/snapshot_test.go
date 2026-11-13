package catalogread

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent access tests here: a snapshot is one transaction, which
// is never shared; concurrent snapshots on one pool are covered by
// upgradecheck's TestRun_ConcurrentOnOnePool.

func TestBeginSnapshot_InvalidInput(t *testing.T) {
	if _, err := BeginSnapshot(context.Background(), nil, "", time.Second); !errors.Is(err,
		ErrNoDB) {
		t.Fatalf("nil db = %v, want ErrNoDB", err)
	}
	pool := testPool(t)
	for _, d := range []time.Duration{0, -time.Second} {
		_, err := BeginSnapshot(context.Background(), pool, "", d)
		if err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Fatalf("timeout %s = %v, want a refusal", d, err)
		}
	}
}

func TestBeginSnapshot_ReadOnlyRepeatableReadBounded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx, err := BeginSnapshot(ctx, pool, "/* pg_sage test */ ", 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("BeginSnapshot: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var st, jit, ro, iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'),
		current_setting('jit'), current_setting('transaction_read_only'),
		current_setting('transaction_isolation')`).Scan(&st, &jit, &ro, &iso); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if st != "1500ms" || jit != "off" || ro != "on" || iso != "repeatable read" {
		t.Fatalf("settings = %s %s %s %s, want 1500ms off on repeatable read", st, jit, ro, iso)
	}
	_, err = tx.Exec(ctx, "CREATE TEMP TABLE snapshot_write_probe (a int)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write in snapshot = %v, want SQLSTATE 25006", err)
	}
}

func TestInSavepoint_FailureKeepsTheTransactionUsable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx, err := BeginSnapshot(ctx, pool, "", time.Second)
	if err != nil {
		t.Fatalf("BeginSnapshot: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sentinel := errors.New("check failed")
	err = InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		if _, err := sp.Exec(ctx, "SELECT 1/0"); err == nil {
			t.Fatal("division by zero did not fail")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InSavepoint = %v, want fn's own error", err)
	}
	var n int
	if err := InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		return sp.QueryRow(ctx, "SELECT 41 + 1").Scan(&n)
	}); err != nil || n != 42 {
		t.Fatalf("after a failed savepoint: n=%d err=%v, want the transaction usable", n, err)
	}
	_ = tx.Rollback(ctx)
	err = InSavepoint(ctx, tx, func(pgx.Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "savepoint") {
		t.Fatalf("savepoint on a closed transaction = %v, want a savepoint error", err)
	}
}
