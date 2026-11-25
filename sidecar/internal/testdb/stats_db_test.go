package testdb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-13 on the shared server: ResetStatements clears only the fixture
// database's pg_stat_statements entries, so packages running in parallel
// keep theirs.

func statsPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// runMarker runs a statement whose text carries marker n times.
func runMarker(t testing.TB, pool *pgxpool.Pool, marker string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(context.Background(),
			"SELECT $1::text AS "+marker, marker); err != nil {
			t.Fatalf("run marker %s: %v", marker, err)
		}
	}
}

// markerCalls is the calls of the entries carrying marker in the database
// that dsn names, read through reader (whose own reads land elsewhere).
func markerCalls(t testing.TB, reader *pgxpool.Pool, dsn, marker string) int64 {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	var calls int64
	err = reader.QueryRow(context.Background(), `SELECT COALESCE(sum(s.calls), 0)::bigint
		FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = $1 AND strpos(s.query, $2) > 0`,
		cfg.ConnConfig.Database, marker).Scan(&calls)
	if err != nil {
		t.Fatalf("read %s entries: %v", marker, err)
	}
	return calls
}

// databaseEntries counts every entry of the database dsn names.
func databaseEntries(t testing.TB, reader *pgxpool.Pool, dsn string) int64 {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	var n int64
	err = reader.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid WHERE d.datname = $1`,
		cfg.ConnConfig.Database).Scan(&n)
	if err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return n
}

func TestResetStatementsClearsOnlyItsDatabase(t *testing.T) {
	dsnA, dsnB := CreateDatabase(t, "pgss_a"), CreateDatabase(t, "pgss_b")
	a, b := statsPool(t, dsnA), statsPool(t, dsnB)
	runMarker(t, a, "ci13_scope_a", 3)
	runMarker(t, b, "ci13_scope_b", 4)
	if got := markerCalls(t, a, dsnA, "ci13_scope_a"); got != 3 {
		t.Fatalf("fixture: database A marker calls = %d, want 3", got)
	}
	entriesB := databaseEntries(t, a, dsnB)
	if got := markerCalls(t, a, dsnB, "ci13_scope_b"); got != 4 || entriesB == 0 {
		t.Fatalf("fixture: database B marker calls = %d (%d entries), want 4", got, entriesB)
	}
	before := readDealloc(t, a)
	dealloc, err := ResetStatements(context.Background(), a)
	if err != nil {
		t.Fatalf("ResetStatements: %v", err)
	}
	// Every read goes through A, so B's entries change only if the reset
	// reaches them (a read's text carries the marker only as a parameter).
	if got := markerCalls(t, a, dsnA, "ci13_scope_a"); got != 0 {
		t.Errorf("database A marker calls after its reset = %d, want 0", got)
	}
	if got := markerCalls(t, a, dsnB, "ci13_scope_b"); got != 4 {
		t.Errorf("database B marker calls after A's reset = %d, want 4 (untouched)", got)
	}
	if got := databaseEntries(t, a, dsnB); got != entriesB {
		t.Errorf("database B entries after A's reset = %d, want %d", got, entriesB)
	}
	// dealloc is server-wide: other packages' evictions move it between
	// reads, so the reset's value lies between the reads around it.
	if after := readDealloc(t, a); dealloc < before || dealloc > after {
		t.Errorf("dealloc = %d, want pg_stat_statements_info's, between %d and %d",
			dealloc, before, after)
	}
}

// readDealloc is pg_stat_statements_info's server-wide eviction count.
func readDealloc(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		"SELECT dealloc FROM pg_stat_statements_info").Scan(&n); err != nil {
		t.Fatalf("read dealloc: %v", err)
	}
	return n
}

// Nil/empty: resetting a database that has nothing left is not an error.
func TestResetStatementsOnAnEmptyDatabaseIsNotAnError(t *testing.T) {
	pool := statsPool(t, CreateDatabase(t, "pgss_empty"))
	for i := 0; i < 2; i++ {
		if _, err := ResetStatements(context.Background(), pool); err != nil {
			t.Fatalf("reset %d: %v", i+1, err)
		}
	}
}

// Invalid input: a database without the extension gets an error naming it.
func TestResetStatementsWithoutTheExtensionOnTheServer(t *testing.T) {
	pool := statsPool(t, CreateDatabase(t, "pgss_none"))
	if _, err := pool.Exec(context.Background(),
		"DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop extension: %v", err)
	}
	dealloc, err := ResetStatements(context.Background(), pool)
	if err == nil || !strings.Contains(err.Error(), "pg_stat_statements") || dealloc != 0 {
		t.Fatalf("ResetStatements = %d, %v; want an error naming pg_stat_statements",
			dealloc, err)
	}
}

const (
	resetScopeWorkerEnv = "PGSAGE_RESET_SCOPE_WORKER"
	resetScopeDSNEnv    = "PGSAGE_RESET_SCOPE_DSN"
	resetScopeFor       = 10 * time.Second
)

var resetCountLine = regexp.MustCompile(`ci13-resets=(\d+)`)

// TestResetScopeWorker is a child process of the concurrency test; in a
// normal run (no worker variable) it returns at once.
func TestResetScopeWorker(t *testing.T) {
	worker := os.Getenv(resetScopeWorkerEnv)
	if worker == "" {
		return
	}
	pool := statsPool(t, os.Getenv(resetScopeDSNEnv))
	deadline := time.Now().Add(resetScopeFor)
	switch worker {
	case "reset":
		resets := 0
		for time.Now().Before(deadline) {
			runMarker(t, pool, "ci13_loop_reset", 1)
			if _, err := ResetStatements(context.Background(), pool); err != nil {
				t.Fatalf("reset %d: %v", resets+1, err)
			}
			resets++
		}
		fmt.Printf("ci13-resets=%d\n", resets)
	case "keep":
		dsn := os.Getenv(resetScopeDSNEnv)
		runMarker(t, pool, "ci13_loop_keep", 5)
		for time.Now().Before(deadline) {
			if got := markerCalls(t, pool, dsn, "ci13_loop_keep"); got != 5 {
				t.Fatalf("own marker calls = %d, want 5: another process's reset reached "+
					"this database", got)
			}
			time.Sleep(200 * time.Millisecond)
		}
	default:
		t.Fatalf("unknown worker %q", worker)
	}
}

// Concurrency: one process resets its database's statements in a loop
// while another, on the same server, asserts its own entries survive.
func TestResetStatementsInParallelProcessesKeepsOtherDatabases(t *testing.T) {
	dsns := map[string]string{
		"reset": CreateDatabase(t, "pgss_loop_reset"),
		"keep":  CreateDatabase(t, "pgss_loop_keep"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmds, outs := map[string]*exec.Cmd{}, map[string]*strings.Builder{}
	for worker, dsn := range dsns {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResetScopeWorker$",
			"-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), resetScopeWorkerEnv+"="+worker,
			resetScopeDSNEnv+"="+dsn)
		outs[worker] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[worker], outs[worker]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %s: %v", worker, err)
		}
		cmds[worker] = cmd
	}
	for worker, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("worker %s: %v\n%s", worker, err, outs[worker])
		}
	}
	m := resetCountLine.FindStringSubmatch(outs["reset"].String())
	if m == nil {
		t.Fatalf("the reset worker printed no reset count:\n%s", outs["reset"])
	}
	if n, _ := strconv.Atoi(m[1]); n < 10 {
		t.Fatalf("the reset worker reset %d times in %s, want at least 10", n, resetScopeFor)
	}
}
