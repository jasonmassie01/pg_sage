package executor

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Helper is invoked only by the parent test with a designated fixture and ID.
// Exit 77 occurs after the durable verdict commit, before lifecycle finalization.
func TestVerifierCrashChildFixture(t *testing.T) {
	dsn := os.Getenv("PREFLIGHT_SHARED_VERIFY_DSN")
	if dsn == "" {
		t.Fatal("crash helper requires an explicit disposable parent fixture")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(database, "pgsage_internal_executor_") {
		t.Fatalf("refusing non-fixture database %q", database)
	}
	id, err := strconv.ParseInt(os.Getenv("PREFLIGHT_VERIFY_ACTION"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, os.Getenv("PREFLIGHT_VERIFY_AT"))
	if err != nil {
		t.Fatal(err)
	}
	f := &preflightFixture{pool: pool, at: at, id: id}
	engine := preflightEngine(t, f, verify.NewPostgresStateStore(pool, 1))
	v, err := engine.Watch(t.Context(), verify.WatchRequest{
		ID: "process-crash", ActionID: id, ExecutedAt: at,
		Table: "public.preflight_items", IndexName: "public.preflight_items_idx",
		Criterion: verify.Criterion{Kind: "per_query_latency", TargetIDs: []int64{id},
			Window: time.Minute, HardMax: 4 * time.Minute},
	})
	if err != nil || !v.Revert || v.Reason != "query_regression" {
		t.Fatalf("cannot crash before valid persisted revert: verdict=%+v err=%v", v, err)
	}
	os.Exit(77)
}

func TestPreflightVerifierProcessCrashResumesRollback(t *testing.T) {
	f := preflightNewFixture(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestVerifierCrashChildFixture$", "-test.v", "-test.timeout=20s")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SAGE_TEST_DATABASE_URL=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "SAGE_TEST_DATABASE_URL=",
		"PREFLIGHT_SHARED_VERIFY_DSN="+os.Getenv("SAGE_TEST_DATABASE_URL"),
		"PREFLIGHT_VERIFY_ACTION="+strconv.FormatInt(f.id, 10),
		"PREFLIGHT_VERIFY_AT="+f.at.Format(time.RFC3339Nano))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("child did not exit at selected crash boundary: err=%v output=%s", err, out)
	}
	t.Log("child process exited 77 immediately after committing the revert verdict")
	fresh := preflightRecoveryEngine(t, f, verify.NewPostgresStateStore(f.pool, 1))
	if err := preflightLifecycle(f, fresh).ResumeDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	preflightAssertRecovered(t, f)
}
