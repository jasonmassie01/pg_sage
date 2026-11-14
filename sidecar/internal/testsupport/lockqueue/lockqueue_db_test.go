package lockqueue

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// observer is a separate session for reading the catalogs. It never reads
// the scenario's table (that would queue behind the DDL), and its statement
// timeout turns a mistake into an error instead of a hang.
func observer(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["statement_timeout"] = "5000"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("observer connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

type relationLock struct {
	pid     int
	mode    string
	granted bool
}

func relationLocks(t *testing.T, conn *pgx.Conn, table string) []relationLock {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT pid, mode, granted FROM pg_locks
		WHERE locktype = 'relation' AND relation = $1::regclass ORDER BY pid`, table)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	locks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (relationLock, error) {
		var l relationLock
		return l, row.Scan(&l.pid, &l.mode, &l.granted)
	})
	if err != nil {
		t.Fatalf("scan pg_locks: %v", err)
	}
	return locks
}

func blockingPids(t *testing.T, conn *pgx.Conn, pid int) []int {
	t.Helper()
	var pids []int
	if err := conn.QueryRow(context.Background(),
		"SELECT pg_blocking_pids($1)", pid).Scan(&pids); err != nil {
		t.Fatalf("pg_blocking_pids(%d): %v", pid, err)
	}
	return pids
}

// checkQueue asserts the pile-up: one granted ACCESS SHARE (the root), and
// exactly readers+1 waiting sessions (the DDL, then the readers behind it).
func checkQueue(t *testing.T, conn *pgx.Conn, s *Scenario, readers int) {
	t.Helper()
	want := map[int]relationLock{s.Root: {s.Root, "AccessShareLock", true},
		s.DDL: {s.DDL, "AccessExclusiveLock", false}}
	for _, pid := range s.Readers {
		want[pid] = relationLock{pid, "AccessShareLock", false}
	}
	if len(s.Readers) != readers || len(want) != readers+2 {
		t.Fatalf("scenario pids = %+v, want %d distinct readers", s, readers)
	}
	locks := relationLocks(t, conn, s.Table)
	waiting := 0
	for _, l := range locks {
		if want[l.pid] != l {
			t.Errorf("lock %+v, want %+v", l, want[l.pid])
		}
		if !l.granted {
			waiting++
		}
	}
	if len(locks) != readers+2 || waiting != readers+1 {
		t.Errorf("locks = %+v: %d waiting, want %d behind one holder", locks, waiting, readers+1)
	}
	if got := blockingPids(t, conn, s.DDL); len(got) != 1 || got[0] != s.Root {
		t.Errorf("DDL blocked by %v, want only the root %d", got, s.Root)
	}
	for _, pid := range s.Readers {
		if got := blockingPids(t, conn, pid); len(got) != 1 || got[0] != s.DDL {
			t.Errorf("reader %d blocked by %v, want only the DDL %d", pid, got, s.DDL)
		}
	}
}

func checkSessions(t *testing.T, conn *pgx.Conn, s *Scenario) {
	t.Helper()
	pids := append([]int{s.Root, s.DDL}, s.Readers...)
	rows, err := conn.Query(context.Background(), `SELECT pid, application_name,
		coalesce(wait_event_type, '') FROM pg_stat_activity WHERE pid = ANY($1)`, pids)
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var pid int
		var app, wait string
		if err := rows.Scan(&pid, &app, &wait); err != nil {
			t.Fatal(err)
		}
		seen++
		if app != AppName || (wait == "Lock") == (pid == s.Root) {
			t.Errorf("pid %d: application %q wait %q", pid, app, wait)
		}
	}
	if seen != len(pids) {
		t.Errorf("pg_stat_activity shows %d of %d sessions", seen, len(pids))
	}
}

// Boundary (0 and 1 readers) and the migration-05 shape (3), in parallel
// on separate tables, so concurrent scenarios do not see each other.
func TestStart_QueuesTheDDLAndReadersBehindOneHolder(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	for _, readers := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("readers=%d", readers), func(t *testing.T) {
			t.Parallel()
			conn := observer(t, dsn)
			table := fmt.Sprintf("public.lq_queue_%d", readers)
			s := Start(t, dsn, table, readers)
			if s.Table != table || !strings.HasPrefix(s.DDLSQL, "ALTER TABLE "+table+" ") {
				t.Errorf("scenario table %q DDL %q", s.Table, s.DDLSQL)
			}
			checkQueue(t, conn, s, readers)
			checkSessions(t, conn, s)
		})
	}
}
