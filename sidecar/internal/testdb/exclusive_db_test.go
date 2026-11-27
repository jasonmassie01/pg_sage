package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// ci-12 integration: these need SAGE_TEST_DATABASE_URL and
// SAGE_TEST_EXCLUSIVE_URL naming two different servers. Other packages
// hold and queue on the same exclusive lock while these run, and
// PostgreSQL grants it to the next waiter the moment it is free, so the
// lock checks follow this package's own sessions by pid in pg_locks.

func exclusiveConn(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func clusterID(t *testing.T, dsn string) int64 {
	t.Helper()
	var id int64
	err := exclusiveConn(t, dsn).QueryRow(context.Background(),
		"SELECT system_identifier FROM pg_control_system()").Scan(&id)
	if err != nil {
		t.Fatalf("read system_identifier: %v", err)
	}
	return id
}

func databaseExists(t *testing.T, base, name string) bool {
	t.Helper()
	var exists bool
	err := exclusiveConn(t, base).QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		t.Fatalf("look up database %s: %v", name, err)
	}
	return exists
}

// exclusiveLockFree reports whether the exclusive lock can be taken now.
func exclusiveLockFree(t *testing.T, base string) bool {
	t.Helper()
	release, ok, err := clusterlock.TryLock(context.Background(), base, exclusiveLockName)
	if err != nil {
		t.Fatalf("try the exclusive lock: %v", err)
	}
	if ok {
		release()
	}
	return ok
}

// exclusiveLockRows restricts pg_locks to the exclusive lock: clusterlock's
// advisory lock on hashtext($1), its key, in the maintenance database $2.
// pg_advisory_lock(bigint) keeps the key's high half in classid and its low
// half in objid, with objsubid 1.
const exclusiveLockRows = "locktype = 'advisory' AND objsubid = 1" +
	" AND database = (SELECT oid FROM pg_database WHERE datname = $2)" +
	" AND ((classid::bigint << 32) | objid::bigint) = hashtext($1)::bigint"

// exclusiveLockArgs returns $1 and $2 of exclusiveLockRows, then extra.
func exclusiveLockArgs(extra ...any) []any {
	return append([]any{clusterlock.Key(exclusiveLockName), clusterlock.MaintenanceDatabase},
		extra...)
}

// exclusiveHolder returns the pid of the session holding the exclusive
// lock. While the caller holds the server through Exclusive, that is the
// caller's own lock session.
func exclusiveHolder(t testing.TB, base string) int {
	t.Helper()
	var pid int
	err := exclusiveConn(t, base).QueryRow(context.Background(),
		"SELECT pid FROM pg_locks WHERE granted AND "+exclusiveLockRows,
		exclusiveLockArgs()...).Scan(&pid)
	if err != nil {
		t.Fatalf("read the exclusive lock's holder: %v", err)
	}
	return pid
}

// releasedBy reports whether session pid let go of the exclusive lock
// within 5s. The wait covers the backend's exit, which finishes after the
// client has hung up. It asks about pid alone: by then another package's
// waiter may hold the lock.
func releasedBy(t *testing.T, base string, pid int) bool {
	t.Helper()
	conn := exclusiveConn(t, base)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var held bool
		if err := conn.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_locks"+
			" WHERE granted AND pid = $3 AND "+exclusiveLockRows+")",
			exclusiveLockArgs(pid)...).Scan(&held); err != nil {
			t.Fatalf("read the exclusive lock's holder: %v", err)
		}
		if !held {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestExclusiveGivesAFreshDatabaseOnTheExclusiveServer(t *testing.T) {
	designated := SkipUnlessLive(t)
	base := exclusiveURL(t, "happy path")
	var name string
	var holder int
	t.Cleanup(func() {
		if name != "" && databaseExists(t, base, name) {
			t.Errorf("fixture database %s still exists after the test", name)
		}
		if holder != 0 && !releasedBy(t, base, holder) {
			t.Errorf("lock session %d still holds the exclusive lock 5s after the test",
				holder)
		}
	})
	t.Run("measure", func(t *testing.T) {
		dsn := Exclusive(t, "happy path")
		holder = exclusiveHolder(t, base)
		conn := exclusiveConn(t, dsn)
		if err := conn.QueryRow(context.Background(),
			"SELECT current_database()").Scan(&name); err != nil {
			t.Fatalf("current_database: %v", err)
		}
		if !fixtureNamePattern.MatchString(name) {
			t.Errorf("current_database %q is not a fixture name", name)
		}
		if parsed, err := url.Parse(dsn); err != nil || parsed.Path != "/"+name {
			t.Errorf("DSN path does not name the fixture %s (parse error %v)", name, err)
		}
		got, exclusive := clusterID(t, dsn), clusterID(t, base)
		if got != exclusive || got == clusterID(t, designated) || got == 0 {
			t.Errorf("fixture is on cluster %d, want the exclusive cluster %d", got, exclusive)
		}
		if exclusiveLockFree(t, base) {
			t.Error("the exclusive lock is free while a test holds the server")
		}
	})
	if name == "" {
		t.Fatal("the measuring subtest never reached its database")
	}
}

func TestExclusiveRefusesTheDesignatedServer(t *testing.T) {
	designated := SkipUnlessLive(t)
	t.Setenv(ExclusiveEnvName, designated)
	returned := false
	rec := runTB(t, func(tb testing.TB) {
		Exclusive(tb, "same cluster probe")
		returned = true
	})
	if returned || len(rec.fatals) != 1 || len(rec.skips) != 0 {
		t.Fatalf("returned=%v fatals=%q skips=%q, want one fatal", returned, rec.fatals, rec.skips)
	}
	if !strings.Contains(rec.fatals[0], "same cluster") {
		t.Errorf("fatal %q does not say same cluster", rec.fatals[0])
	}
}

// A test body that fails still releases the server: its lock session lets
// go with the test's cleanups, after its database is dropped, and the next
// caller can hold the server. Other packages may take the lock first, so
// the test checks its own session's pid, not that the lock is free or how
// long the next caller waits.
func TestExclusiveReleasesTheServerWhenTheTestFails(t *testing.T) {
	SkipUnlessLive(t)
	base := exclusiveURL(t, "lock release probe")
	var held string
	var holder int
	rec := runTB(t, func(tb testing.TB) {
		held = Exclusive(tb, "lock release probe")
		holder = exclusiveHolder(tb, base)
		tb.Fatal("the measured body failed")
	})
	if held == "" || holder == 0 || len(rec.fatals) != 1 || len(rec.errors) != 0 {
		t.Fatalf("held=%q holder=%d fatals=%q errors=%q, want one fatal after Exclusive "+
			"returned", held, holder, rec.fatals, rec.errors)
	}
	if !releasedBy(t, base, holder) {
		t.Fatalf("lock session %d still holds the exclusive lock 5s after the failed test",
			holder)
	}
	parsed, err := url.Parse(held)
	if err != nil {
		t.Fatalf("parse the held DSN: %v", err)
	}
	if databaseExists(t, base, strings.TrimPrefix(parsed.Path, "/")) {
		t.Errorf("the failed test's database %s was not dropped", parsed.Path)
	}
	t.Run("next caller", func(t *testing.T) { Exclusive(t, "lock release probe") })
}

const (
	overlapWorkerEnv = "PGSAGE_EXCLUSIVE_OVERLAP_WORKER"
	overlapTableEnv  = "PGSAGE_EXCLUSIVE_OVERLAP_TABLE"
	overlapRounds    = 2
	overlapHold      = time.Second
)

// TestExclusiveOverlapWorker is the child process of the overlap test; in
// a normal run (no worker variable) it returns at once.
func TestExclusiveOverlapWorker(t *testing.T) {
	worker := os.Getenv(overlapWorkerEnv)
	if worker == "" {
		return
	}
	base := exclusiveURL(t, "overlap worker")
	table := pgx.Identifier{os.Getenv(overlapTableEnv)}.Sanitize()
	for round := 0; round < overlapRounds; round++ {
		t.Run("round", func(t *testing.T) {
			Exclusive(t, "overlap worker")
			conn := exclusiveConn(t, base)
			ctx := context.Background()
			var id int64
			if err := conn.QueryRow(ctx, "INSERT INTO "+table+
				" (worker, started) VALUES ($1, clock_timestamp()) RETURNING id",
				worker).Scan(&id); err != nil {
				t.Fatalf("record start: %v", err)
			}
			time.Sleep(overlapHold)
			if _, err := conn.Exec(ctx, "UPDATE "+table+
				" SET ended = clock_timestamp() WHERE id = $1", id); err != nil {
				t.Fatalf("record end: %v", err)
			}
		})
	}
}

func overlapTable(t *testing.T, base string) string {
	t.Helper()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	name := "pgsage_exclusive_overlap_" + hex.EncodeToString(suffix)
	ident := pgx.Identifier{name}.Sanitize()
	conn := exclusiveConn(t, base)
	if _, err := conn.Exec(context.Background(), "CREATE TABLE "+ident+
		" (id bigserial PRIMARY KEY, worker text NOT NULL,"+
		" started timestamptz NOT NULL, ended timestamptz)"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = exclusiveConn(t, base).Exec(context.Background(), "DROP TABLE "+ident)
	})
	return name
}

// Two test processes started together, each holding the exclusive server
// twice, never hold it at the same time.
func TestExclusiveSerializesConcurrentProcesses(t *testing.T) {
	SkipUnlessLive(t)
	base := exclusiveURL(t, "overlap probe")
	table := overlapTable(t, base)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workers := []string{"a", "b"}
	cmds := make([]*exec.Cmd, len(workers))
	outs := make([]*strings.Builder, len(workers))
	for i, worker := range workers {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExclusiveOverlapWorker$",
			"-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), overlapWorkerEnv+"="+worker, overlapTableEnv+"="+table)
		outs[i] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[i], outs[i]
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %s: %v", workers[i], err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker %s: %v\n%s", workers[i], err, outs[i])
		}
	}
	checkDisjoint(t, base, table, len(workers))
}

func checkDisjoint(t *testing.T, base, table string, workers int) {
	t.Helper()
	rows, err := exclusiveConn(t, base).Query(context.Background(),
		"SELECT worker, started, ended FROM "+pgx.Identifier{table}.Sanitize()+
			" ORDER BY started")
	if err != nil {
		t.Fatalf("read intervals: %v", err)
	}
	type interval struct {
		worker     string
		start, end *time.Time
	}
	var got []interval
	perWorker := map[string]int{}
	for rows.Next() {
		var iv interval
		if err := rows.Scan(&iv.worker, &iv.start, &iv.end); err != nil {
			t.Fatalf("scan interval: %v", err)
		}
		got = append(got, iv)
		perWorker[iv.worker]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read intervals: %v", err)
	}
	if len(got) != workers*overlapRounds || len(perWorker) != workers {
		t.Fatalf("recorded %d intervals from %v, want %d from %d workers",
			len(got), perWorker, workers*overlapRounds, workers)
	}
	for i, iv := range got {
		if iv.end == nil {
			t.Fatalf("interval %d of worker %s never recorded its end", i, iv.worker)
		}
		// The server's clock_timestamp() may run slightly off the worker's
		// sleep (a VM's clock is slewed), so half the hold proves a real hold.
		if held := iv.end.Sub(*iv.start); held < overlapHold/2 {
			t.Fatalf("interval %d of worker %s held for %s, want about %s",
				i, iv.worker, held, overlapHold)
		}
		if i > 0 && got[i-1].end.After(*iv.start) {
			t.Errorf("worker %s held the server from %s while worker %s held it until %s",
				iv.worker, iv.start.Format(time.RFC3339Nano), got[i-1].worker,
				got[i-1].end.Format(time.RFC3339Nano))
		}
	}
}

// lockWaiter opens a session in the exclusive server's maintenance
// database, where clusterlock takes the exclusive lock, and returns it with
// its pid, so the drop-order test can tell its own wait from other
// packages' Exclusive callers queued on the same lock.
func lockWaiter(t testing.TB, base string) (*pgx.Conn, int) {
	t.Helper()
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse %s: not a PostgreSQL connection URL", ExclusiveEnvName)
	}
	cfg.Database = clusterlock.MaintenanceDatabase
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect the lock waiter: %v", err)
	}
	return conn, int(conn.PgConn().PID())
}

// waitForLockWaiter returns once session pid waits for the exclusive lock,
// with the number of other sessions waiting for it in the same pg_locks
// snapshot. Any of those may be ahead of pid in the queue; a session that
// starts waiting later queues behind it.
func waitForLockWaiter(t testing.TB, conn *pgx.Conn, pid int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var mine, others int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FILTER (WHERE pid = $3),"+
			" count(*) FILTER (WHERE pid <> $3) FROM pg_locks WHERE NOT granted AND "+
			exclusiveLockRows, exclusiveLockArgs(pid)...).Scan(&mine, &others); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if mine > 0 {
			return others
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %d did not wait for the exclusive lock within 10s", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// existsOnAcquire queues waiter on the exclusive lock with Exclusive's
// budget, as an Exclusive caller does, and once it holds the lock reports
// at once, on the same session, whether database name still exists.
// Closing waiter releases the lock.
func existsOnAcquire(waiter *pgx.Conn, name func() string) (bool, error) {
	defer func() { _ = waiter.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), exclusiveLockWait)
	defer cancel()
	if _, err := waiter.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		clusterlock.Key(exclusiveLockName)); err != nil {
		return false, err
	}
	var exists bool
	err := waiter.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)",
		name()).Scan(&exists)
	return exists, err
}

// waiterResult is what the drop-order test's waiting session found when it
// got the exclusive lock.
type waiterResult struct {
	exists bool
	err    error
}

// queueWaiter queues the test's own session on the exclusive lock behind
// the caller, which holds it, and returns the session's pid once it waits.
// The channel then gets whether database name existed when the session got
// the lock.
func queueWaiter(tb testing.TB, base string, name func() string) (int, <-chan waiterResult) {
	tb.Helper()
	poll := exclusiveConn(tb, base)
	waiter, pid := lockWaiter(tb, base)
	acquired := make(chan waiterResult, 1)
	go func() {
		exists, err := existsOnAcquire(waiter, name)
		acquired <- waiterResult{exists, err}
	}()
	if others := waitForLockWaiter(tb, poll, pid); others > 0 {
		tb.Logf("%d other sessions also waited for the exclusive server; if one is ahead of "+
			"session %d, it holds the server next and this run does not test the drop order",
			others, pid)
	}
	return pid, acquired
}

// The next holder must not find the last test's database still there:
// DROP DATABASE writes WAL and forces a checkpoint, which the next
// measuring test would otherwise count as its own. Other packages queue on
// the same lock, so the test watches its own waiting session by pid, and
// that session may wait behind them as long as an Exclusive caller would.
func TestExclusiveDropsTheDatabaseBeforeReleasingTheServer(t *testing.T) {
	SkipUnlessLive(t)
	base := exclusiveURL(t, "release order probe")
	var mu sync.Mutex
	var name string
	fixture := func() string { mu.Lock(); defer mu.Unlock(); return name }
	var pid int
	var acquired <-chan waiterResult
	rec := runTB(t, func(tb testing.TB) {
		parsed, err := url.Parse(Exclusive(tb, "release order probe"))
		if err != nil {
			tb.Fatalf("parse the held DSN: %v", err)
		}
		mu.Lock()
		name = strings.TrimPrefix(parsed.Path, "/")
		mu.Unlock()
		pid, acquired = queueWaiter(tb, base, fixture)
	})
	if len(rec.fatals) != 0 || len(rec.errors) != 0 || fixture() == "" {
		t.Fatalf("name=%q fatals=%q errors=%q, want a clean hold", fixture(), rec.fatals,
			rec.errors)
	}
	select {
	case got := <-acquired:
		if got.err != nil {
			t.Fatalf("the waiting session %d: %v", pid, got.err)
		}
		if got.exists {
			t.Errorf("the next holder found %s still present: the server was released "+
				"before the database was dropped", fixture())
		}
	case <-time.After(exclusiveLockWait + time.Minute):
		t.Fatalf("the waiting session %d never got the exclusive lock", pid)
	}
}
