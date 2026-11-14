//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// apiauth-14: run with the production dependencies against the real schema.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "cmd/create_admin"))
}

// bootstrappedDB returns the fixture DSN and a pool on a bootstrapped schema
// with no users and a fresh sage.users id sequence.
func bootstrappedDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap the sage schema: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"TRUNCATE sage.users RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("empty sage.users: %v", err)
	}
	return dsn, pool
}

func runCreate(dsn, email, password string) (int, string) {
	var stderr bytes.Buffer
	code := run(adminEnv(dsn, email, password), &stderr, productionAdminDeps())
	return code, stderr.String()
}

func TestRunCreatesAdminOnBootstrappedSchema(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	code, out := runCreate(dsn, "Ops@Example.com", goodPassword)
	if code != 0 {
		t.Fatalf("run = %d; stderr %q", code, out)
	}
	var (
		id         int
		email      string
		role, hash string
		mustChange bool
		rows       int
	)
	err := pool.QueryRow(context.Background(), `SELECT id, email, role, password,
		must_change_password, (SELECT count(*) FROM sage.users) FROM sage.users`).
		Scan(&id, &email, &role, &hash, &mustChange, &rows)
	if err != nil {
		t.Fatalf("read the created admin: %v", err)
	}
	if rows != 1 || email != "ops@example.com" || role != "admin" || mustChange {
		t.Fatalf("users=%d email=%q role=%q must_change=%v", rows, email, role, mustChange)
	}
	if !strings.HasPrefix(hash, "$2") || !auth.CheckPassword(hash, goodPassword) {
		t.Fatalf("stored password %q is not a bcrypt hash of the password", hash)
	}
	if !strings.Contains(out, fmt.Sprintf("Created admin user id=%d", id)) {
		t.Fatalf("stderr %q does not report id %d", out, id)
	}
	if strings.Contains(out, goodPassword) {
		t.Fatalf("stderr leaks the password: %q", out)
	}
}

// A restored dump can leave the sequence behind the data: setval moves it
// past the highest id before the insert.
func TestRunContinuesAfterAManualInsert(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.users
		(id, email, password, role) VALUES (41, 'restored@example.com', 'x', 'viewer')`)
	if err != nil {
		t.Fatalf("insert id 41: %v", err)
	}
	code, out := runCreate(dsn, "admin@example.com", goodPassword)
	if code != 0 {
		t.Fatalf("run = %d; stderr %q", code, out)
	}
	if id := userID(t, pool, "admin@example.com"); id != 42 {
		t.Fatalf("admin id = %d, want 42", id)
	}
}

func TestRunReportsAMissingUsersTable(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "no_sage_schema")
	code, out := runCreate(dsn, "", goodPassword)
	if code != 1 {
		t.Fatalf("run = %d, want 1; stderr %q", code, out)
	}
	if !strings.HasPrefix(out, "reset sage.users sequence: ") {
		t.Fatalf("stderr = %q, want the sequence step", out)
	}
}

func TestConcurrentRunsCreateDistinctAdmins(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	const n = 6
	var wg sync.WaitGroup
	codes, outs := make([]int, n), make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outs[i] = runCreate(dsn, fmt.Sprintf("admin%d@example.com", i),
				goodPassword)
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Errorf("run %d = %d; stderr %q", i, codes[i], outs[i])
		}
	}
	var rows, distinct int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*), count(DISTINCT id) FROM sage.users").Scan(&rows, &distinct)
	if err != nil || rows != n || distinct != n {
		t.Fatalf("users = %d distinct ids = %d (err %v), want %d", rows, distinct, err, n)
	}
}

// Another writer (the sidecar's users API, or a second run) holds an id from
// the sequence in an open transaction. setval must not hand that id out
// again: a rewind made this run wait on the row, then fail on the duplicate.
func TestRunDoesNotRewindBelowAnUncommittedInsert(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var held int
	if err := tx.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ('api@example.com', 'x', 'viewer') RETURNING id`).Scan(&held); err != nil {
		t.Fatalf("hold an id: %v", err)
	}
	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() {
		code, out := runCreate(dsn, "admin@example.com", goodPassword)
		done <- result{code, out}
	}()
	var res result
	select {
	case res = <-done:
	case <-waitForLockWait(t, pool):
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the held row: %v", err)
		}
		res = <-done
	case <-time.After(60 * time.Second):
		t.Fatal("run neither finished nor waited on a lock within 60 seconds")
	}
	if res.code != 0 {
		t.Fatalf("run = %d; stderr %q", res.code, res.out)
	}
	if id := userID(t, pool, "admin@example.com"); id == held {
		t.Fatalf("admin reused the held id %d", held)
	}
}

// A run that holds the create_admin lock (from setval to its committed insert)
// makes the next run wait for it rather than read a stale sequence.
func TestRunWaitsWhileAnotherRunHoldsTheSequenceLock(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	ctx := context.Background()
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))",
		sequenceLockKey); err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := runCreate(dsn, "admin@example.com", goodPassword)
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("run finished (%d) while another run held the lock", code)
	case <-waitForLockWait(t, pool):
	case <-time.After(60 * time.Second):
		t.Fatal("run did not wait on the lock within 60 seconds")
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))",
		sequenceLockKey); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("run after the lock was released = %d", code)
	}
	if id := userID(t, pool, "admin@example.com"); id != 1 {
		t.Fatalf("admin id = %d, want 1", id)
	}
}

// A run holds the lock session while auth.CreateUser inserts on a second
// one, so a DSN that caps the pool at one connection used to wait forever.
func TestRunFinishesWithAOneConnectionPoolDSN(t *testing.T) {
	dsn, pool := bootstrappedDB(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	done := make(chan [2]string, 1)
	go func() {
		code, out := runCreate(dsn+sep+"pool_max_conns=1", "one@example.com", goodPassword)
		done <- [2]string{fmt.Sprint(code), out}
	}()
	select {
	case res := <-done:
		if res[0] != "0" {
			t.Fatalf("run = %s; stderr %q", res[0], res[1])
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run with pool_max_conns=1 did not finish within 60 seconds")
	}
	if id := userID(t, pool, "one@example.com"); id != 1 {
		t.Fatalf("admin id = %d, want 1", id)
	}
}

// waitForLockWait closes its channel once a session in this database waits on
// a lock, and never closes it if none does within 20 seconds.
func waitForLockWait(t *testing.T, pool *pgxpool.Pool) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			err := pool.QueryRow(context.Background(), `SELECT count(*)
				FROM pg_stat_activity WHERE datname = current_database()
				AND wait_event_type = 'Lock'`).Scan(&waiting)
			if err == nil && waiting > 0 {
				close(ch)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return ch
}

func userID(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		"SELECT id FROM sage.users WHERE email = $1", email).Scan(&id); err != nil {
		t.Fatalf("find %s: %v", email, err)
	}
	return id
}
