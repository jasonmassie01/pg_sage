package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-q: verification targets and write latency read
// pg_stat_statements in schema ext through a session pinned to
// pg_catalog, pg_temp.
func TestExecutorReadersFindExtensionOutsideSearchPath(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "executorext")
	if _, err := admin.Exec(ctx, "CREATE TABLE public.ext_probe (v int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	const probe = "SELECT count(*) FROM public.ext_probe"
	id := testdb.SeedStatement(t, admin, pool, probe, 3)
	testdb.SeedStatement(t, admin, pool,
		"INSERT INTO public.ext_probe SELECT v FROM public.ext_probe", 2)
	var logged []string
	e := &Executor{pool: pool, logFn: func(_, format string, _ ...any) {
		logged = append(logged, format)
	}}
	ids := e.workloadIDs(ctx, tableStatementsSQL, wordPattern("ext_probe"), verifyTargetLimit)
	if !containsID(ids, id) || len(logged) != 0 {
		t.Fatalf("workloadIDs = %v (logged %q), want queryid %d", ids, logged, id)
	}
	state := e.snapshotBeforeState(ctx, nil)
	if v, ok := state["mean_exec_time_ms"].(float64); !ok || v <= 0 {
		t.Fatalf("before state = %v, want a positive write latency", state)
	}
}

// Nil/empty: without the extension no target is selected and the cause
// is logged, never a bare-name read.
func TestWorkloadIDsWithoutExtension(t *testing.T) {
	ctx := context.Background()
	admin, pool := testdb.ExtSchemaDatabase(t, "executornoext")
	if _, err := admin.Exec(ctx, "DROP EXTENSION pg_stat_statements"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	var logged []string
	e := &Executor{pool: pool, logFn: func(_, format string, _ ...any) {
		logged = append(logged, format)
	}}
	if ids := e.workloadIDs(ctx, tableStatementsSQL, "x", 1); ids != nil || len(logged) != 1 {
		t.Fatalf("workloadIDs = %v (logged %q), want nil and one log line", ids, logged)
	}
}
