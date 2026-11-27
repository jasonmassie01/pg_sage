package lockqueue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func liveSessions(t *testing.T, conn *pgx.Conn, pids []int) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)", pids).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

func tableExists(t *testing.T, conn *pgx.Conn, table string) bool {
	t.Helper()
	var exists bool
	if err := conn.QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return exists
}

// State transition: when the test ends every session ends and the table is
// dropped, so the next test starts with no queue.
func TestStart_CleanupEndsSessionsAndDropsTheTable(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	conn := observer(t, dsn)
	const table = "public.lq_cleanup"
	var pids []int
	t.Run("pile-up", func(t *testing.T) {
		s := Start(t, dsn, table, 2)
		pids = append([]int{s.Root, s.DDL}, s.Readers...)
		if got := liveSessions(t, conn, pids); got != 4 {
			t.Fatalf("live sessions during the test = %d, want 4", got)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for liveSessions(t, conn, pids) != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := liveSessions(t, conn, pids); got != 0 {
		t.Errorf("%d scenario sessions outlived the test", got)
	}
	if tableExists(t, conn, table) {
		t.Errorf("%s outlived the test", table)
	}
}

// Start replaces a leftover table of the same name, so a crashed earlier
// run cannot change the shape of the queue.
func TestStart_ReplacesALeftoverTable(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	conn := observer(t, dsn)
	const table = "public.lq_leftover"
	if _, err := conn.Exec(context.Background(),
		"CREATE TABLE "+table+" (stale text, id int)"); err != nil {
		t.Fatal(err)
	}
	s := Start(t, dsn, table, 1)
	var cols []string
	// Catalog reads take no lock on the table, so they do not queue.
	if err := conn.QueryRow(context.Background(), `SELECT array_agg(attname::text
		ORDER BY attnum) FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0
		AND NOT attisdropped`, table).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0] != "id" {
		t.Errorf("columns = %v, want [id] (a fresh table)", cols)
	}
	checkQueue(t, conn, s, 1)
}

// Error propagation: SQL the server rejects fails the test with the
// statement in the message, and the admin session is still closed.
func TestStart_ReportsARejectedStatement(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	fatals, cleanups := runFatal(func(tb testing.TB) {
		Start(tb, dsn, "lq bad name", 0)
	})
	if len(fatals) != 1 || !strings.HasPrefix(fatals[0],
		"lockqueue: DROP TABLE IF EXISTS lq bad name:") ||
		!strings.Contains(fatals[0], "SQLSTATE 42601") {
		t.Fatalf("fatals = %q, want the rejected DROP with its syntax error", fatals)
	}
	if cleanups != 1 {
		t.Errorf("cleanups = %d, want 1 (the admin session)", cleanups)
	}
}
