package cases

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Correctness and boundary tests of the case diagnostics on real
// PostgreSQL (playbooks-04).

// blockInBackground runs sql on conn until the test ends, then cancels it.
func blockInBackground(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.Exec(ctx, sql)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// Correctness: CREATE INDEX CONCURRENTLY waits on the virtualxid of one
// open writer. The lock diagnostic names exactly that writer, not every
// session that holds a virtualxid (an idle transaction and the
// diagnostic's own read-only transaction hold one too).
func TestLockDiagnosticReportsTheOneCICBlocker(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	diagExec(t, admin, "DROP TABLE IF EXISTS case_diag_cic")
	diagExec(t, admin, "CREATE TABLE case_diag_cic (id int)")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP TABLE IF EXISTS case_diag_cic")
	})
	writer := diagConnect(t, dsn, "")
	diagExec(t, writer, "BEGIN")
	diagExec(t, writer, "INSERT INTO case_diag_cic VALUES (1)")
	bystander := diagConnect(t, dsn, "")
	diagExec(t, bystander, "BEGIN")
	diagExec(t, bystander, "SELECT txid_current()")
	cic := diagConnect(t, dsn, "")
	diagExec(t, cic, "SET lock_timeout = '60s'")
	cicPID := diagPID(t, cic)
	blockInBackground(t, cic, "CREATE INDEX CONCURRENTLY case_diag_cic_idx ON case_diag_cic (id)")
	waitForCount(t, admin, 1, `SELECT count(*) FROM pg_stat_activity
		WHERE pid = $1 AND wait_event_type = 'Lock'`, cicPID)

	rows := runDiagnostic(t, admin, "diagnose_lock_blockers")[probes.LockGraph]

	if len(rows) != 1 {
		t.Fatalf("lock diagnostic = %d rows %v, want exactly the writer", len(rows), rows)
	}
	if got := mustInt(t, rows[0], "blocker_pid"); got != diagPID(t, writer) {
		t.Fatalf("blocker_pid = %d, want the writer %d", got, diagPID(t, writer))
	}
	if got := mustInt(t, rows[0], "waiter_pid"); got != cicPID {
		t.Fatalf("waiter_pid = %d, want the CREATE INDEX CONCURRENTLY %d", got, cicPID)
	}
	if rows[0]["lock_type"] != "virtualxid" {
		t.Fatalf("lock_type = %v, want virtualxid", rows[0]["lock_type"])
	}
}

// openLockFlood opens holders sessions with ACCESS SHARE on a table and
// waiters sessions queued for ACCESS EXCLUSIVE behind them: waiter k is
// blocked by every holder and every earlier waiter, so the wait graph
// has more than holders*waiters edges.
func openLockFlood(t *testing.T, dsn string, admin *pgx.Conn, holders, waiters int) {
	t.Helper()
	diagExec(t, admin, "DROP TABLE IF EXISTS case_diag_flood")
	diagExec(t, admin, "CREATE TABLE case_diag_flood (id int)")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP TABLE IF EXISTS case_diag_flood")
	})
	for i := 0; i < holders; i++ {
		h := diagConnect(t, dsn, "")
		diagExec(t, h, "BEGIN")
		diagExec(t, h, "LOCK TABLE case_diag_flood IN ACCESS SHARE MODE")
	}
	pids := make([]int64, 0, waiters)
	for i := 0; i < waiters; i++ {
		w := diagConnect(t, dsn, "")
		diagExec(t, w, "SET lock_timeout = '60s'")
		diagExec(t, w, "BEGIN")
		pids = append(pids, diagPID(t, w))
		blockInBackground(t, w, "LOCK TABLE case_diag_flood IN ACCESS EXCLUSIVE MODE")
	}
	waitForCount(t, admin, int64(waiters), `SELECT count(*) FROM pg_stat_activity
		WHERE pid = ANY ($1) AND wait_event_type = 'Lock'`, pids)
}

// Boundary: 20 holders and 30 queued waiters make over 1000 wait edges;
// the lock diagnostic returns exactly lock_graph's MaxRows of them.
func TestLockDiagnosticIsCappedAtMaxRows(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	openLockFlood(t, dsn, admin, 20, 30)

	rows := runDiagnostic(t, admin, "diagnose_lock_blockers")[probes.LockGraph]

	spec, _ := probes.Catalog().Spec(probes.LockGraph)
	if len(rows) != spec.MaxRows {
		t.Fatalf("lock diagnostic = %d rows, want exactly the cap %d", len(rows), spec.MaxRows)
	}
}

// Boundary: 105 snapshot holders; the freeze diagnostic lists exactly
// xmin_horizon's cap of 100.
func TestFreezeDiagnosticIsCappedAtItsSpecRows(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	for i := 0; i < 105; i++ {
		c := diagConnect(t, dsn, "")
		diagExec(t, c, "BEGIN ISOLATION LEVEL REPEATABLE READ")
		diagExec(t, c, "SELECT 1")
	}
	waitForCount(t, admin, 105, `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND backend_xmin IS NOT NULL
		  AND pid <> pg_backend_pid()`, diagApp)

	rows := runDiagnostic(t, admin, "diagnose_freeze_blockers")[probes.XminHorizon]

	spec, _ := probes.Catalog().Spec(probes.XminHorizon)
	if spec.MaxRows != 100 || len(rows) != spec.MaxRows {
		t.Fatalf("freeze diagnostic = %d rows, want exactly the cap %d", len(rows),
			spec.MaxRows)
	}
}

// Happy path: an inactive physical slot that reserved WAL shows its
// retained bytes and WAL status.
func TestWALDiagnosticShowsInactiveSlotRetention(t *testing.T) {
	admin := diagConnect(t, guardedDSN(t), "")
	const slot = "case_diag_inactive_slot"
	_, _ = admin.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	diagExec(t, admin, "SELECT pg_create_physical_replication_slot($1, true)", slot)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
	})

	rows := runDiagnostic(t, admin, "diagnose_wal_replication")[probes.ReplicationSlots]

	got := rowsWith(rows, "slot_name", slot)
	if len(got) != 1 {
		t.Fatalf("WAL diagnostic slot rows = %v, want %s once", rows, slot)
	}
	if got[0]["active"] != false {
		t.Fatalf("slot active = %v, want false", got[0]["active"])
	}
	if retained, ok := asInt64(got[0]["retained_bytes"]); !ok || retained < 0 {
		t.Fatalf("retained_bytes = %#v, want a non-null byte count", got[0]["retained_bytes"])
	}
	if got[0]["wal_status"] == nil {
		t.Fatal("wal_status is null for a slot that reserved WAL")
	}
}

// Happy path (cases-09): a session that holds only an XID (no snapshot
// once its statement ends) holds the horizon back and is listed.
func TestFreezeDiagnosticListsAnXIDOnlySession(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	holder := diagConnect(t, dsn, "")
	diagExec(t, holder, "BEGIN")
	diagExec(t, holder, "SELECT txid_current()")
	pid := diagPID(t, holder)

	rows := runDiagnostic(t, admin, "diagnose_freeze_blockers")[probes.XminHorizon]

	got := rowsWith(rows, "pid", pid)
	if len(got) != 1 || got[0]["holder_kind"] != "session" {
		t.Fatalf("freeze diagnostic rows for pid %d = %v, want one session row", pid, got)
	}
}

// Happy path (cases-09): a prepared transaction holds the horizon back
// and appears in the freeze diagnostic, named by the hash of its gid.
func TestFreezeDiagnosticListsAPreparedTransaction(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	var maxPrepared int
	if err := admin.QueryRow(context.Background(),
		"SELECT current_setting('max_prepared_transactions')::int").Scan(&maxPrepared); err != nil {
		t.Fatalf("read max_prepared_transactions: %v", err)
	}
	if maxPrepared == 0 {
		t.Skip("max_prepared_transactions = 0: PREPARE TRANSACTION is unavailable here")
	}
	const gid = "case_diag_prepared"
	_, _ = admin.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'")
	prep := diagConnect(t, dsn, "")
	diagExec(t, prep, "BEGIN")
	diagExec(t, prep, "SELECT txid_current()")
	diagExec(t, prep, "PREPARE TRANSACTION '"+gid+"'")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'")
	})

	rows := runDiagnostic(t, admin, "diagnose_freeze_blockers")[probes.XminHorizon]

	sum := md5.Sum([]byte(gid))
	got := rowsWith(rows, "holder_name", hex.EncodeToString(sum[:]))
	if len(got) != 1 || got[0]["holder_kind"] != "prepared_xact" {
		t.Fatalf("freeze diagnostic = %v, want the prepared transaction once", rows)
	}
}
