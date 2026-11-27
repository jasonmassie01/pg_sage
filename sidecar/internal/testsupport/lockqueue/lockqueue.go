// Package lockqueue builds the lock-queue pile-up behind an in-flight DDL
// on a real server, for tests: a root session holds ACCESS SHARE on a
// table (an active pg_sleep reading it), a DDL session waits for ACCESS
// EXCLUSIVE behind it, and readers queue behind the DDL (migration-05).
package lockqueue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// AppName is the application_name of the scenario's sessions.
const AppName = "lockqueue_app"

// Scenario is one pile-up; the fields are backend pids.
type Scenario struct {
	Table   string
	Root    int
	DDL     int
	Readers []int
	// DDLSQL is the statement the DDL session waits in.
	DDLSQL string
}

type session struct {
	conn   *pgx.Conn
	cancel context.CancelFunc
	done   chan error
}

// Start creates table (one row) and the pile-up with readers queued
// behind the DDL. Every session ends, and the table is dropped, when the
// test ends.
func Start(t testing.TB, dsn, table string, readers int) *Scenario {
	t.Helper()
	admin := connect(t, dsn)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	mustExec(t, admin, "DROP TABLE IF EXISTS "+table)
	mustExec(t, admin, "CREATE TABLE "+table+" (id int)")
	mustExec(t, admin, "INSERT INTO "+table+" VALUES (1)")
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	s := &Scenario{Table: table,
		DDLSQL: "ALTER TABLE " + table + " ADD COLUMN lockqueue_extra int"}
	s.Root = start(t, dsn, "SELECT pg_sleep(60) FROM "+table)
	await(t, admin, s.Root, grantedSQL, table)
	s.DDL = start(t, dsn, s.DDLSQL)
	await(t, admin, s.DDL, waitingSQL)
	for i := 0; i < readers; i++ {
		pid := start(t, dsn, "SELECT count(*) FROM "+table)
		await(t, admin, pid, waitingSQL)
		s.Readers = append(s.Readers, pid)
	}
	return s
}

const grantedSQL = `SELECT EXISTS (SELECT 1 FROM pg_locks
	WHERE pid = $1 AND granted AND relation = $2::regclass)`

const waitingSQL = `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
	WHERE pid = $1 AND wait_event_type = 'Lock')`

func connect(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("lockqueue: parse DSN: %v", err)
	}
	cfg.RuntimeParams["application_name"] = AppName
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("lockqueue: connect: %v", err)
	}
	return conn
}

func mustExec(t testing.TB, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("lockqueue: %s: %v", sql, err)
	}
}

// start runs sql on a new session in the background and returns its pid;
// the statement is cancelled and the session closed at cleanup.
func start(t testing.TB, dsn, sql string) int {
	t.Helper()
	conn := connect(t, dsn)
	var pid int
	if err := conn.QueryRow(context.Background(),
		"SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("lockqueue: backend pid: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{conn: conn, cancel: cancel, done: make(chan error, 1)}
	go func() {
		_, err := conn.Exec(ctx, sql)
		s.done <- err
	}()
	t.Cleanup(func() { s.stop(t) })
	return pid
}

func (s *session) stop(t testing.TB) {
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("lockqueue: session ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Logf("lockqueue: session did not end within 10s")
	}
	_ = s.conn.Close(context.Background())
}

// await polls check (with pid and extra args) until it reports true.
func await(t testing.TB, admin *pgx.Conn, pid int, check string, extra ...any) {
	t.Helper()
	args := append([]any{pid}, extra...)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		if err := admin.QueryRow(context.Background(), check, args...).
			Scan(&ok); err != nil {
			t.Fatalf("lockqueue: await pid %d: %v", pid, err)
		}
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lockqueue: pid %d did not reach %q within 10s", pid, check)
}
