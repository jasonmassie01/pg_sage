package verify

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// ci-17: VACUUM of an ordinary table only waits for snapshots in its own
// database (PostgreSQL ignores other databases' backend_xmin outside the
// shared catalogs), but waitSnapshotsPastNow waited for every backend of
// the cluster. In a full round another package's database on the shared
// server held a snapshot past 30 s and TestIndexFootprintBeforeAndAfterReindex
// failed without any snapshot that could hold back its VACUUM.
//
// No concurrency or state-transition cases beyond the held snapshot: the
// helper is a bounded poll over pg_stat_activity.

// holdSnapshotIn opens a REPEATABLE READ transaction on dsn and keeps its
// snapshot (backend_xmin) until t ends.
func holdSnapshotIn(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the snapshot holder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin the snapshot holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("take the holder's snapshot: %d, %v", one, err)
	}
}

// Integration: a snapshot held in another database of the same server
// neither delays the wait nor keeps the VACUUM from removing the deleted
// rows, so the footprint test passes with it held.
func TestIndexFootprintIgnoresASnapshotInAnotherDatabase(t *testing.T) {
	other := testdb.CreateDatabase(t, "verify_snapshot_holder")
	holdSnapshotIn(t, other)
	pool := verifyIntegrationPool(t)
	started := time.Now()
	waitSnapshotsPastNow(t, t.Context(), pool)
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("waited %s for a snapshot in another database", waited)
	}
	t.Run("footprint", TestIndexFootprintBeforeAndAfterReindex)
}

// Boundary: a snapshot held in the test's own database still holds the
// wait back (the helper must keep waiting for what VACUUM waits for).
func TestWaitSnapshotsPastNowSeesASnapshotInItsOwnDatabase(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "verify_snapshot_same")
	holdSnapshotIn(t, dsn)
	pool := verifyPoolOn(t, dsn)
	clear, err := snapshotsPastNow(t.Context(), pool, currentXID(t, pool))
	if err != nil {
		t.Fatalf("read the xmin horizon: %v", err)
	}
	if clear {
		t.Fatal("a snapshot held in the same database was not seen")
	}
}

// Error propagation: a cancelled context surfaces as an error, never as
// "clear".
func TestSnapshotsPastNowCancelledContextIsAnError(t *testing.T) {
	pool := verifyIntegrationPool(t)
	now := currentXID(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clear, err := snapshotsPastNow(ctx, pool, now)
	if err == nil || clear {
		t.Fatalf("cancelled read = %v, %v; want an error and not clear", clear, err)
	}
}

// Invalid input: an empty transaction id is refused (PG 14 would read it
// as transaction id 0 and report every snapshot clear).
func TestSnapshotsPastNowRejectsAnEmptyXID(t *testing.T) {
	pool := verifyIntegrationPool(t)
	clear, err := snapshotsPastNow(t.Context(), pool, "")
	if err == nil || clear {
		t.Fatalf("empty xid = %v, %v; want an error and not clear", clear, err)
	}
}

func currentXID(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) string {
	t.Helper()
	var now string
	if err := pool.QueryRow(t.Context(), "SELECT pg_current_xact_id()::text").
		Scan(&now); err != nil || now == "" {
		t.Fatalf("read the current transaction id: %q, %v", now, err)
	}
	return now
}
