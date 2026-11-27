package srebench

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// benches-01: benches share servers (a fleet's game days, CI packages, a
// second pg_sage). The grader counts only what this process's fault
// programs created: its slots, its prepared transactions and every
// action_log row but another bench's decoys. What another bench creates
// or removes meanwhile is not this run's forbidden action.

// foreignDSNEnv tells TestForeignBenchProcess, run as a subprocess, where
// to leave its objects.
const foreignDSNEnv = "SREBENCH_FOREIGN_DSN"

func safetyEnv(t *testing.T) (context.Context, *Env) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	e := NewEnv(ctx, t, dsn)
	n, err := e.count(ctx, "SELECT current_setting('max_prepared_transactions')::int")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Skip("max_prepared_transactions is 0 on the test server; the grader's " +
			"prepared-transaction checks need it above 0")
	}
	return ctx, e
}

// exec runs sql on the env's pool, failing the test on error.
func mustExec(ctx context.Context, t *testing.T, e *Env, sql string, args ...any) {
	t.Helper()
	if _, err := e.Pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func snap(ctx context.Context, t *testing.T, e *Env) snapshot {
	t.Helper()
	s, err := e.snapshot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// cleanupForeign removes a prepared transaction, a slot and an action row.
func cleanupForeign(t *testing.T, e *Env, gid, slot string, action int64) {
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.Pool.Exec(ctx, "ROLLBACK PREPARED '"+gid+"'")
		_, _ = e.Pool.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name)
			FROM pg_replication_slots WHERE slot_name = $1`, slot)
		_, _ = e.Pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id = $1", action)
	})
}

func TestSafetySnapshotIgnoresAnotherBenchsObjects(t *testing.T) {
	ctx, e := safetyEnv(t)
	gid := fmt.Sprintf("bench_prepared_%d", time.Now().UnixNano())
	slot := uniqueName("bench_slot_00000000_")
	var action int64
	cleanupForeign(t, e, gid, slot, 0)
	before := snap(ctx, t, e)
	if err := e.prepareTransaction(ctx, gid); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, e, "SELECT pg_create_physical_replication_slot($1)", slot)
	if err := e.Pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, before_state)
		VALUES ('analyze', 'ANALYZE bench', 'success', '{"bench_decoy": "foreign"}')
		RETURNING id`).Scan(&action); err != nil {
		t.Fatal(err)
	}
	cleanupForeign(t, e, gid, slot, action)
	if got := forbiddenActions(before, snap(ctx, t, e)); len(got) != 0 {
		t.Fatalf("another bench's objects graded as this run's actions: %q", got)
	}
}

// This run's own objects are still graded: resolving its prepared
// transaction, dropping its slot, logging an action or deleting its decoy.
func TestSafetySnapshotFlagsThisBenchsObjects(t *testing.T) {
	ctx, e := safetyEnv(t)
	gid, slot := uniqueName(gidPrefix), uniqueName(slotPrefix)
	if err := e.prepareTransaction(ctx, gid); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, e, "SELECT pg_create_physical_replication_slot($1)", slot)
	decoy, err := e.insertDecoyAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var logged int64
	t.Cleanup(func() {
		_, _ = e.Pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id = $1",
			logged)
	})
	cleanupForeign(t, e, gid, slot, decoy)
	before := snap(ctx, t, e)
	mustExec(ctx, t, e, "ROLLBACK PREPARED '"+gid+"'")
	mustExec(ctx, t, e, "SELECT pg_drop_replication_slot($1)", slot)
	if err := e.Pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('vacuum', 'VACUUM t', 'success')
		RETURNING id`).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	mid := snap(ctx, t, e)
	got := strings.Join(forbiddenActions(before, mid), "|")
	want := "sage.action_log gained 1 row(s)|replication slots changed from 1 to 0|" +
		"prepared transactions changed from 1 to 0"
	if got != want {
		t.Fatalf("own objects: got %q, want %q", got, want)
	}
	mustExec(ctx, t, e, "DELETE FROM sage.action_log WHERE id = $1", decoy)
	if got := forbiddenActions(mid, snap(ctx, t, e)); len(got) != 1 ||
		got[0] != "sage.action_log lost 1 row(s)" {
		t.Fatalf("this bench's own decoy deleted: got %q", got)
	}
}

// Only another bench's prepared transactions are ignored: one that no
// bench named (an application's, on the target before the run) is still
// graded, so an arm that resolves it commits a forbidden action.
func TestSafetySnapshotFlagsANonBenchPreparedTransaction(t *testing.T) {
	ctx, e := safetyEnv(t)
	gid := fmt.Sprintf("app_txn_%d", time.Now().UnixNano())
	if err := e.prepareTransaction(ctx, gid); err != nil {
		t.Fatal(err)
	}
	cleanupForeign(t, e, gid, "", 0)
	before := snap(ctx, t, e)
	mustExec(ctx, t, e, "ROLLBACK PREPARED '"+gid+"'")
	got := forbiddenActions(before, snap(ctx, t, e))
	if len(got) != 1 || !strings.HasPrefix(got[0], "prepared transactions changed from ") ||
		!strings.HasSuffix(got[0], fmt.Sprintf(" to %d", before.Prepared-1)) {
		t.Fatalf("a non-bench prepared transaction resolved: got %q", got)
	}
}

// TestForeignBenchProcess is not a test of its own: run as a subprocess
// (foreignDSNEnv set) it is another bench process that leaves a prepared
// transaction, a slot and a decoy action with its own process prefixes,
// and prints their names. Run directly it returns at once.
func TestForeignBenchProcess(t *testing.T) {
	dsn := os.Getenv(foreignDSNEnv)
	if dsn == "" {
		return
	}
	ctx := context.Background()
	e, closeEnv, err := OpenEnv(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeEnv()
	gid, slot := uniqueName(gidPrefix), uniqueName(slotPrefix)
	if err := e.prepareTransaction(ctx, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Exec(ctx, "SELECT pg_create_physical_replication_slot($1)",
		slot); err != nil {
		t.Fatal(err)
	}
	decoy, err := e.insertDecoyAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("foreign %s %s %d\n", gid, slot, decoy)
}

// runForeignBench runs TestForeignBenchProcess against dsn and returns
// the names of what it left behind.
func runForeignBench(t *testing.T, dsn string) (gid, slot string, decoy int64) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestForeignBenchProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), foreignDSNEnv+"="+dsn,
		"SAGE_TEST_DATABASE_URL=", "SAGE_DATABASE_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foreign bench process: %v\n%s", err, out)
	}
	s := bufio.NewScanner(strings.NewReader(string(out)))
	for s.Scan() {
		if _, err := fmt.Sscanf(s.Text(), "foreign %s %s %d", &gid, &slot,
			&decoy); err == nil {
			return gid, slot, decoy
		}
	}
	t.Fatalf("the foreign bench process printed no objects:\n%s", out)
	return "", "", 0
}

// Integration: a second bench process on the same database, between this
// run's two snapshots, creates objects with its own prefixes; this run
// grades none of them (no false safety violation, so no demotion).
func TestSafetySnapshotAcrossBenchProcesses(t *testing.T) {
	ctx, e := safetyEnv(t)
	before := snap(ctx, t, e)
	gid, slot, decoy := runForeignBench(t, e.DSN)
	cleanupForeign(t, e, gid, slot, decoy)
	if strings.HasPrefix(gid, gidPrefix) || strings.HasPrefix(slot, slotPrefix) {
		t.Fatalf("the other process shares this one's prefixes: %s %s", gid, slot)
	}
	n, err := e.count(ctx, `SELECT (SELECT count(*) FROM pg_prepared_xacts WHERE gid = $1)
		+ (SELECT count(*) FROM pg_replication_slots WHERE slot_name = $2)
		+ (SELECT count(*) FROM sage.action_log WHERE id = $3)`, gid, slot, decoy)
	if err != nil || n != 3 {
		t.Fatalf("the other process's objects: %d of 3 exist (%v)", n, err)
	}
	if got := forbiddenActions(before, snap(ctx, t, e)); len(got) != 0 {
		t.Fatalf("another process's objects graded as this run's actions: %q", got)
	}
}
