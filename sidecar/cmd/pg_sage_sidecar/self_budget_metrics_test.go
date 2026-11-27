package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// The declared self-budget is exported with what pg_sage uses: the
// sidecar's CPU per collector cycle (a process figure from the process
// watch, unlabelled, footprint-07), each database's database time and
// blocks per hour, the budgets, whether each known resource is over
// budget (CPU once for the process), and the busy time of each loop with
// its database as a label of its own.
func TestWriteSelfBudgetMetrics(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"zeta": {DBKnown: true, DBTimeMsPerHour: 5000,
			BlocksPerHour: 70_000, StorageKnown: true, StorageBytes: 2 << 30},
		"alpha": {StorageKnown: true, StorageBytes: 4096}, // first cycle
	}, selfbudget.CPUReading{Known: true, MsPerCycle: 900},
		selfbudget.Budget{CPUMsPerCycle: 600, DBTimeMsPerHour: 180_000,
			BlocksPerHour: 18_000_000, StorageBytes: 1 << 30}, 0,
		map[string]selfbudget.LoopStat{
			"collector": {Busy: 90 * time.Second, Runs: 30},
			"analyzer":  {Busy: 1500 * time.Millisecond, Runs: 3},
		})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_self_cpu_ms_per_cycle gauge",
		"pg_sage_self_cpu_ms_per_cycle 900",
		`pg_sage_self_db_time_ms_per_hour{database="zeta"} 5000`,
		`pg_sage_self_blocks_per_hour{database="zeta"} 70000`,
		`pg_sage_self_budget{resource="cpu"} 600`,
		`pg_sage_self_budget{resource="db_time"} 180000`,
		`pg_sage_self_budget{resource="io"} 18000000`,
		`pg_sage_self_budget{resource="storage"} 1073741824`,
		`pg_sage_self_budget_exceeded{resource="cpu"} 1`,
		`pg_sage_self_budget_exceeded{database="zeta",resource="db_time"} 0`,
		`pg_sage_self_budget_exceeded{database="zeta",resource="storage"} 1`,
		`pg_sage_self_budget_exceeded{database="alpha",resource="storage"} 0`,
		"# TYPE pg_sage_self_loop_busy_seconds_total counter",
		`pg_sage_self_loop_busy_seconds_total{loop="collector"} 90`,
		`pg_sage_self_loop_busy_seconds_total{loop="analyzer"} 1.5`,
		`pg_sage_self_loop_runs_total{loop="collector"} 30`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{
		`pg_sage_self_db_time_ms_per_hour{database="alpha"}`,
		`pg_sage_self_budget_exceeded{database="alpha",resource="cpu"}`,
		`pg_sage_self_budget_exceeded{database="zeta",resource="cpu"}`,
		`pg_sage_self_budget_exceeded{database="alpha",resource="db_time"}`,
	} {
		if strings.Contains(out, absent) {
			t.Errorf("unknown value exported: %q", absent)
		}
	}
	if strings.Index(out, `{loop="analyzer"}`) > strings.Index(out, `{loop="collector"}`) {
		t.Error("loops are not exported in sorted order")
	}
}

// A disabled resource has no budget series and is never "exceeded".
func TestWriteSelfBudgetMetrics_DisabledResources(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"app": {StorageKnown: true, StorageBytes: 1 << 40},
	}, selfbudget.CPUReading{Known: true, MsPerCycle: 1e9}, selfbudget.Budget{}, 0, nil)
	out := b.String()
	if strings.Contains(out, "pg_sage_self_budget{") ||
		strings.Contains(out, "pg_sage_self_budget_exceeded{") {
		t.Fatalf("disabled budget exported:\n%s", out)
	}
	if !strings.Contains(out, "pg_sage_self_cpu_ms_per_cycle 1000000000") {
		t.Fatalf("usage must still be exported:\n%s", out)
	}
}

func TestWriteSelfBudgetMetrics_NothingKnownWritesNothing(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, nil, selfbudget.CPUReading{},
		selfbudget.Budget{CPUMsPerCycle: 600}, 0, nil)
	if b.Len() != 0 {
		t.Fatalf("nothing measured wrote %q", b.String())
	}
}

// Nil/empty: no databases at all, the process CPU is still exported with
// its exceeded flag (footprint-07: the gauge comes from the watch).
func TestWriteSelfBudgetMetrics_ProcessCPUWithoutDatabases(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, nil, selfbudget.CPUReading{Known: true, MsPerCycle: 300},
		selfbudget.Budget{CPUMsPerCycle: 600}, 0, nil)
	out := b.String()
	for _, want := range []string{"pg_sage_self_cpu_ms_per_cycle 300",
		`pg_sage_self_budget{resource="cpu"} 600`,
		`pg_sage_self_budget_exceeded{resource="cpu"} 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// footprint-07: a loop keyed component:database is exported with the
// component as loop and the database as its own label, so sum by (loop)
// adds every database; llm_wait is a loop of its own.
func TestWriteSelfBudgetMetrics_LoopsByDatabase(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, nil, selfbudget.CPUReading{}, selfbudget.Budget{}, 0,
		map[string]selfbudget.LoopStat{
			"executor:db1":   {Busy: 2 * time.Second, Runs: 4},
			"llm_wait:db1":   {Busy: 30 * time.Second, Runs: 2},
			"collector:zeta": {Busy: 3 * time.Second, Runs: 3},
			"notify":         {Busy: time.Second, Runs: 1},
		})
	out := b.String()
	for _, want := range []string{
		`pg_sage_self_loop_busy_seconds_total{loop="executor",database="db1"} 2`, // CHECK-11
		`pg_sage_self_loop_runs_total{loop="executor",database="db1"} 4`,
		`pg_sage_self_loop_busy_seconds_total{loop="llm_wait",database="db1"} 30`,
		`pg_sage_self_loop_busy_seconds_total{loop="collector",database="zeta"} 3`,
		`pg_sage_self_loop_runs_total{loop="notify"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `loop="executor:db1"`) {
		t.Errorf("loop label carries the database:\n%s", out)
	}
}
