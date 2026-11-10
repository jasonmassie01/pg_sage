package collector

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// rca-03 on real PostgreSQL: the system read carries the WAL insert
// position with its read time, the longest idle-in-transaction session and
// the sessions counter, without the advisor's configuration snapshot.

func TestRateInputsDB_WALPositionAdvancesWithItsReadTime(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	ctx := context.Background()
	first := collectAndPublish(t, c)
	if _, err := pool.Exec(ctx, `INSERT INTO public.iv_t
		SELECT g, repeat('w', 1000) FROM generate_series(1, 3000) g`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	second := collectAndPublish(t, c)
	var serverLSN int64
	const lsnSQL = `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::bigint`
	if err := pool.QueryRow(ctx, lsnSQL).Scan(&serverLSN); err != nil {
		t.Fatalf("server LSN: %v", err)
	}
	if first.System.WALLSN <= 0 || first.System.WALReadAt.IsZero() {
		t.Fatalf("first WAL position %d at %v, want a read position", first.System.WALLSN,
			first.System.WALReadAt)
	}
	if d := second.System.WALLSN - first.System.WALLSN; d < 3000*1000 {
		t.Errorf("WAL advanced %d bytes over a 3 MB insert, want at least 3,000,000", d)
	}
	if second.System.WALLSN > serverLSN {
		t.Errorf("snapshot LSN %d is ahead of the server's later %d", second.System.WALLSN,
			serverLSN)
	}
	if !second.System.WALReadAt.After(first.System.WALReadAt) ||
		second.System.WALReadAt.Before(second.CollectedAt) {
		t.Errorf("read times %v -> %v (collected %v), want increasing and not before "+
			"the snapshot", first.System.WALReadAt, second.System.WALReadAt, second.CollectedAt)
	}
}

// One session idle in transaction for ~3 s among two fresh ones: the
// maximum is the old one's, not the average.
func TestRateInputsDB_IdleInTxMaxDuration(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	ctx := context.Background()
	dsn := pool.Config().ConnString()
	open := func() {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	open()
	time.Sleep(3 * time.Second)
	open()
	open()
	snap := collectAndPublish(t, c)
	if snap.System.IdleInTransaction != 3 {
		t.Fatalf("idle in transaction = %d, want 3", snap.System.IdleInTransaction)
	}
	if m := snap.System.IdleInTxMaxSeconds; m < 2.5 || m > 60 {
		t.Errorf("longest idle in transaction = %v s, want the ~3 s session", m)
	}
}

// Fifteen connections opened and closed between two snapshots are counted
// although none survives.
func TestRateInputsDB_SessionsCountClosedConnections(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	ctx := context.Background()
	// PG 14's statistics collector creates the new database's entry (and
	// its stats_reset, the epoch) only after the first report: let it land.
	ivWorkload(t, pool)
	collectAndPublish(t, c)
	for i := 0; i < 15; i++ {
		conn, err := pgx.Connect(ctx, pool.Config().ConnString())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if err := conn.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	time.Sleep(1500 * time.Millisecond) // exits report their session counts
	snap := collectAndPublish(t, c)
	iv := snap.System.Interval
	if iv == nil || iv.Sessions == nil {
		t.Fatalf("interval %+v, want measured sessions", iv)
	}
	if *iv.Sessions < 15 || *iv.Sessions > 40 {
		t.Errorf("sessions this interval = %d, want the 15 short connections", *iv.Sessions)
	}
	if snap.System.Sessions < 15 {
		t.Errorf("lifetime sessions = %d, want at least 15", snap.System.Sessions)
	}
}
