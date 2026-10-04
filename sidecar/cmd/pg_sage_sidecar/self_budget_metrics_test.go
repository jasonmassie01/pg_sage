package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// The declared self-budget is exported with what pg_sage uses: the
// sidecar's CPU per collector cycle (a process figure, unlabelled), each
// database's database time and blocks per hour, the budgets, whether each
// known resource is over budget, and the busy time of each loop.
func TestWriteSelfBudgetMetrics(t *testing.T) {
	var b strings.Builder
	writeSelfBudgetMetrics(&b, map[string]selfbudget.Usage{
		"zeta": {CPUKnown: true, CPUMsPerCycle: 900, DBKnown: true, DBTimeMsPerHour: 5000,
			BlocksPerHour: 70_000, StorageKnown: true, StorageBytes: 2 << 30},
		"alpha": {StorageKnown: true, StorageBytes: 4096}, // first cycle
	}, selfbudget.Budget{CPUMsPerCycle: 600, DBTimeMsPerHour: 180_000,
		BlocksPerHour: 18_000_000, StorageBytes: 1 << 30},
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
		`pg_sage_self_budget_exceeded{database="zeta",resource="cpu"} 1`,
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
		"app": {CPUKnown: true, CPUMsPerCycle: 1e9, StorageKnown: true, StorageBytes: 1 << 40},
	}, selfbudget.Budget{}, nil)
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
	writeSelfBudgetMetrics(&b, nil, selfbudget.Budget{CPUMsPerCycle: 600}, nil)
	if b.Len() != 0 {
		t.Fatalf("nothing measured wrote %q", b.String())
	}
}
