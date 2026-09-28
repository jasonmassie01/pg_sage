package rca

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A test-process restart exercises production constructors with no shared Go memory.
// TestMain sees an empty designated DSN in the child and creates no extra database;
// only this helper receives the explicitly scoped parent fixture DSN.
func TestRCAChildProcessFixture(t *testing.T) {
	dsn := os.Getenv("PREFLIGHT_SHARED_RCA_DSN")
	if dsn == "" {
		// Adapted: a whole-package run also selects this helper. It has
		// no parent fixture then, so it reports a skip instead of failing
		// the package; its parents (TestPreflightRCAProcessRestart*) assert.
		t.Skip("helper process: runs only when a TestPreflightRCAProcessRestart* " +
			"parent passes PREFLIGHT_SHARED_RCA_DSN")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var database string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, "pgsage_internal_rca_") {
		t.Fatalf("refusing non-fixture database %q", database)
	}
	cfg := preflightRCAConfig()
	e := NewEngine(&cfg.RCA, noopLog)
	hot := os.Getenv("PREFLIGHT_RCA_MODE") == "hot"
	cycles := 8
	if hot {
		cycles = 1
	}
	for range cycles {
		preflightRCACycle(t, e, pool, hot)
	}
	if active, _ := preflightRCAState(t, pool); hot && active == 0 {
		t.Fatal("hot child never persisted its incident")
	}
}

func preflightRCAChild(t *testing.T, mode string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestRCAChildProcessFixture$", "-test.v", "-test.timeout=20s")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SAGE_TEST_DATABASE_URL=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "SAGE_TEST_DATABASE_URL=",
		"PREFLIGHT_SHARED_RCA_DSN="+os.Getenv("SAGE_TEST_DATABASE_URL"),
		"PREFLIGHT_RCA_MODE="+mode)
	out, err := cmd.CombinedOutput()
	// Adapted: under `go test -cover -v` the child's own "coverage:" line,
	// echoed in this log, was taken as the package figure (96% reported
	// as 30%). Rename it in the echo; the child's output is otherwise kept.
	echo := strings.ReplaceAll(string(out), "coverage: ", "coverage (child) ")
	t.Logf("fresh RCA process mode=%s: %s", mode, echo)
	if err != nil {
		t.Fatalf("RCA child process failed: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: TestRCAChildProcessFixture") {
		t.Fatal("RCA child process did not run its fixture (skipped or filtered)")
	}
}

func TestPreflightRCAProcessRestartContinuesIdentity(t *testing.T) {
	pool := preflightRCAPool(t)
	preflightRCAChild(t, "hot")
	preflightRCAChild(t, "hot")
	if active, total := preflightRCAState(t, pool); active != 1 || total != 1 {
		t.Fatalf("two OS processes duplicated persistent incident: active=%d total=%d", active, total)
	}
}

func TestPreflightRCAProcessRestartClearsIncident(t *testing.T) {
	pool := preflightRCAPool(t)
	preflightRCAChild(t, "hot")
	preflightRCAChild(t, "clear")
	if active, _ := preflightRCAState(t, pool); active != 0 {
		t.Fatalf("fresh OS process left %d active incident after eight healthy cycles", active)
	}
}
