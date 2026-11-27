package verify

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
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

// holdIdleTx opens a transaction on dsn at iso, runs sql in it and leaves it
// idle until t ends. It returns the backend's pid.
func holdIdleTx(t *testing.T, dsn string, iso pgx.TxIsoLevel, sql string) uint32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, sql); err != nil {
		t.Fatalf("holder %s: %v", sql, err)
	}
	return conn.PgConn().PID()
}

// holdSnapshotIn opens a REPEATABLE READ transaction on dsn and keeps its
// snapshot (backend_xmin) until t ends.
func holdSnapshotIn(t *testing.T, dsn string) {
	t.Helper()
	holdIdleTx(t, dsn, pgx.RepeatableRead, "SELECT 1")
}

// Integration: a snapshot held in another database of the same server
// neither delays the wait nor keeps the VACUUM from removing the deleted
// rows, so the footprint check passes with it held. Both databases are on
// the exclusive server, so that snapshot is the only other one there.
func TestIndexFootprintIgnoresASnapshotInAnotherDatabase(t *testing.T) {
	pool := verifyExclusivePool(t,
		"the footprint's REINDEX must face only the snapshot this test holds")
	holdSnapshotIn(t, testdb.ExclusiveDatabase(t, "verify_snapshot_holder"))
	started := time.Now()
	waitSnapshotsPastNow(t, t.Context(), pool)
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("waited %s for a snapshot in another database", waited)
	}
	checkReindexFootprint(t, pool)
}

// Boundary: a writer idle in its transaction holds no snapshot, only its
// transaction id, and VACUUM keeps every row that transaction may see. The
// wait must see it, on a server where nothing else holds one back.
func TestSnapshotsPastNowSeesAnIdleWritersXID(t *testing.T) {
	dsn := testdb.Exclusive(t, "the wait must be clear but for the writer this test holds")
	pool := verifyPoolOn(t, dsn)
	waitSnapshotsPastNow(t, t.Context(), pool)
	pid := holdIdleTx(t, dsn, pgx.ReadCommitted, "SELECT pg_current_xact_id()")
	var noSnapshot, hasXID bool
	if err := pool.QueryRow(t.Context(), `SELECT backend_xmin IS NULL,
		backend_xid IS NOT NULL FROM pg_stat_activity WHERE pid = $1`, int64(pid)).
		Scan(&noSnapshot, &hasXID); err != nil || !noSnapshot || !hasXID {
		t.Fatalf("writer %d: no snapshot %v, xid %v (%v); want an xid and no snapshot",
			pid, noSnapshot, hasXID, err)
	}
	clear, err := snapshotsPastNow(t.Context(), pool, currentXID(t, pool))
	if err != nil {
		t.Fatalf("read the xmin horizon: %v", err)
	}
	if clear {
		t.Fatal("a writer idle in its transaction was not seen")
	}
}

// Boundary: PostgreSQL 14 counts a backend still logging in in every
// database's horizon, and before 18 no view lists it until it has logged
// in; only its virtualxid lock shows (18 lists it with no database and its
// snapshot). The wait must see it, on a server where nothing else holds
// one back.
func TestSnapshotsPastNowSeesABackendStillLoggingIn(t *testing.T) {
	pool := verifyExclusivePool(t, "the wait must be clear but for the login this test stalls")
	waitSnapshotsPastNow(t, t.Context(), pool)
	stallLogin(t, pool.Config().ConnConfig)
	clear, err := snapshotsPastNow(t.Context(), pool, currentXID(t, pool))
	if err != nil {
		t.Fatalf("read the xmin horizon: %v", err)
	}
	if clear {
		t.Fatal("a backend still logging in was not seen")
	}
}

// stallLogin starts a login to the server cc names and stops once the
// server asks for a password, so that backend stays logging in until t ends
// (or authentication_timeout passes, 60 s by default).
func stallLogin(t *testing.T, cc *pgx.ConnConfig) {
	t.Helper()
	port := strconv.Itoa(int(cc.Port))
	network, addr := "tcp", net.JoinHostPort(cc.Host, port)
	if strings.HasPrefix(cc.Host, "/") {
		network, addr = "unix", filepath.Join(cc.Host, ".s.PGSQL."+port)
	}
	conn, err := net.DialTimeout(network, addr, 10*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the login deadline: %v", err)
	}
	params := "user\x00" + cc.User + "\x00database\x00" + cc.Database + "\x00\x00"
	msg := binary.BigEndian.AppendUint32(nil, uint32(8+len(params)))
	msg = binary.BigEndian.AppendUint32(msg, 3<<16) // protocol 3.0
	if _, err := conn.Write(append(msg, params...)); err != nil {
		t.Fatalf("send the startup message: %v", err)
	}
	head := make([]byte, 9) // message type, length, authentication code
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("read the server's answer to the login: %v", err)
	}
	switch {
	case head[0] != 'R':
		t.Fatalf("the server answered the login with message %q, not a password request",
			head[0])
	case binary.BigEndian.Uint32(head[5:]) == 0:
		t.Skipf("the server trusts %s logins without a password: no login to stall", network)
	}
}

// Boundary: a snapshot held in the test's own database still holds the
// wait back (the helper must keep waiting for what VACUUM waits for), on a
// server where nothing else holds one back: on the shared one, any login
// in flight would hold the wait back as well.
func TestWaitSnapshotsPastNowSeesASnapshotInItsOwnDatabase(t *testing.T) {
	dsn := testdb.Exclusive(t, "the wait must be clear but for the snapshot this test holds")
	pool := verifyPoolOn(t, dsn)
	waitSnapshotsPastNow(t, t.Context(), pool)
	holdSnapshotIn(t, dsn)
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
