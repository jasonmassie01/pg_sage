package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// observability-06: each periodic loop's failed runs and last success.

// No concurrent access tests here: the families render a snapshot copy;
// the tracker's locking is tested in selfbudget.

func renderLoops(loops map[string]selfbudget.LoopStat) string {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, nil, selfbudget.CPUReading{}, selfbudget.Budget{}, 0, loops)
	return b.String()
}

// Happy path and nil/zero: every loop has an error counter (0 included);
// the last success is omitted until the loop's first success. Busy time
// and runs are unchanged.
func TestWriteSelfLoopErrorsAndLastSuccess(t *testing.T) {
	ok := time.Unix(1_700_000_123, 0)
	out := renderLoops(map[string]selfbudget.LoopStat{
		"collector": {Busy: 90 * time.Second, Runs: 30, Errors: 4, LastSuccess: ok},
		"sre":       {Busy: time.Second, Runs: 2, Errors: 2},
		"analyzer":  {Busy: time.Second, Runs: 1, LastSuccess: ok.Add(time.Second)},
	})
	for _, want := range []string{
		`pg_sage_self_loop_busy_seconds_total{loop="collector"} 90`,
		`pg_sage_self_loop_runs_total{loop="collector"} 30`,
		"# TYPE pg_sage_self_loop_errors_total counter",
		`pg_sage_self_loop_errors_total{loop="collector"} 4`,
		`pg_sage_self_loop_errors_total{loop="sre"} 2`,
		`pg_sage_self_loop_errors_total{loop="analyzer"} 0`,
		"# TYPE pg_sage_self_loop_last_success_timestamp_seconds gauge",
		`pg_sage_self_loop_last_success_timestamp_seconds{loop="collector"} 1700000123`,
		`pg_sage_self_loop_last_success_timestamp_seconds{loop="analyzer"} 1700000124`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `pg_sage_self_loop_last_success_timestamp_seconds{loop="sre"}`) {
		t.Errorf("a loop that never succeeded exports a last success:\n%s", out)
	}
	lintScrape(t, out)
}

// Nil/empty: no loop ran, no loop family (not even HELP).
func TestWriteSelfLoopsAbsentWithoutLoops(t *testing.T) {
	if out := renderLoops(nil); strings.Contains(out, "pg_sage_self_loop") {
		t.Fatalf("loop families without loops:\n%s", out)
	}
}

// Boundary: when no loop has succeeded yet the last-success family is
// left out entirely, header included.
func TestWriteSelfLoopsNoSuccessNoLastSuccessFamily(t *testing.T) {
	out := renderLoops(map[string]selfbudget.LoopStat{"sre": {Runs: 1, Errors: 1}})
	if strings.Contains(out, "pg_sage_self_loop_last_success_timestamp_seconds") {
		t.Fatalf("last-success family without a success:\n%s", out)
	}
	if !strings.Contains(out, `pg_sage_self_loop_errors_total{loop="sre"} 1`) {
		t.Fatalf("errors missing:\n%s", out)
	}
}

// fakeCleaner is a retention cleaner that counts its runs.
type fakeCleaner struct{ runs int }

func (f *fakeCleaner) Run(context.Context) { f.runs++ }

// State transitions: the executor and retention loops are tracked by the
// workers themselves (Executor.RunCycle, Cleaner.RunOnce, keyed by
// database: loop_tracking_test.go), so the orchestrator records no run of
// its own; with fake workers no executor or retention run is counted,
// and a real cycle is never counted twice.
func TestFleetDBCycleLeavesLoopTrackingToTheWorkers(t *testing.T) {
	m, _ := scriptedHAMonitor(false)
	before := selfbudget.Process().Snapshot()
	cleaner := &fakeCleaner{}
	exec := &recordingExecutor{}
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "orders", ha: m,
		exec: exec, cleaner: cleaner})
	after := selfbudget.Process().Snapshot()
	for _, loop := range []string{"executor", "retention", "executor:orders",
		"retention:orders"} {
		if got := after[loop].Runs - before[loop].Runs; got != 0 {
			t.Errorf("the orchestrator recorded %d %s runs, want 0", got, loop)
		}
	}
	if cleaner.runs != 1 || len(exec.cycles) != 1 {
		t.Fatalf("cleaner ran %d times and the executor %d, want 1 each", cleaner.runs,
			len(exec.cycles))
	}
}
