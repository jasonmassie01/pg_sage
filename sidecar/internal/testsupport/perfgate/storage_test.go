package perfgate

import (
	"strings"
	"testing"
	"time"
)

// Partitions are charged to their partitioned table: counters are
// differenced per partition and summed; a partition created in the phase
// counts from zero, one dropped in the phase only lowers the size growth.
func TestTableStatsDeltaRollsPartitionsUp(t *testing.T) {
	const qs = "sage.query_store"
	before := TableStats{
		"sage.query_store_history":   {Parent: qs, LiveRows: 100, Written: 100, Bytes: 4096},
		"sage.query_store_p20261001": {Parent: qs, LiveRows: 50, Written: 50, Bytes: 8192},
		"sage.findings": {LiveRows: 10, Written: 20, Updated: 10, HotUpdated: 2,
			Bytes: 1000},
	}
	after := TableStats{
		"sage.query_store_history":   {Parent: qs, LiveRows: 100, Written: 130, Bytes: 4096},
		"sage.query_store_p20261003": {Parent: qs, LiveRows: 7, Written: 7, Bytes: 16384},
		"sage.findings": {LiveRows: 10, Written: 50, Updated: 40, HotUpdated: 29,
			Bytes: 1500},
	}
	got := after.Delta(before)
	want := []TableDelta{
		{Name: "sage.findings", LiveRows: 10, RowsWritten: 30, Updates: 30, HotUpdates: 27,
			Bytes: 1500, BytesGrowth: 500, Relations: 1},
		{Name: "sage.query_store", LiveRows: 107, RowsWritten: 37, Bytes: 20480,
			BytesGrowth: 20480 - 12288, Relations: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("delta = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHotGateFlagsTablesWithFewHeapOnlyUpdates(t *testing.T) {
	b := DefaultBudgets()
	p := steadyPhase()
	p.Tables = []TableDelta{
		{Name: "sage.findings", Updates: 120, HotUpdates: 0},
		{Name: "sage.sre_change_feed_state", Updates: 60, HotUpdates: 29},
		{Name: "sage.just_enough", Updates: 60, HotUpdates: 30},
		{Name: "sage.too_few_updates", Updates: b.HotMinUpdates - 1, HotUpdates: 0},
	}
	got, err := Evaluate([]Phase{p}, b)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("offenders = %+v, want findings and change feed state", got)
	}
	// Worst (0% HOT) ranks first.
	if got[0].Subject != "sage.findings" || got[0].Gate != GateHotUpdates ||
		got[0].Measured != 0 || got[0].Budget != 50 {
		t.Fatalf("first offender = %+v", got[0])
	}
	if got[1].Subject != "sage.sre_change_feed_state" || got[1].Measured < 48.3 ||
		got[1].Measured > 48.4 {
		t.Fatalf("second offender = %+v", got[1])
	}
}

// The HOT share is a property of the schema and the writers, not of load:
// warmup (where the analyzer refreshes every finding it opened) counts.
func TestHotGateAppliesToEveryPhase(t *testing.T) {
	warm := Phase{Name: "warmup", Window: time.Minute}
	warm.Tables = []TableDelta{{Name: "sage.findings", Updates: 500, HotUpdates: 10}}
	got, err := Evaluate([]Phase{warm}, DefaultBudgets())
	if err != nil || len(got) != 1 || got[0].Gate != GateHotUpdates || got[0].Measured != 2 {
		t.Fatalf("warmup gate F = %+v (%v), want findings at 2%% HOT", got, err)
	}
}

func TestBudgetsRejectNonPositiveHotBudgets(t *testing.T) {
	for _, mutate := range []func(*Budgets){
		func(b *Budgets) { b.HotUpdateMinPct = 0 },
		func(b *Budgets) { b.HotMinUpdates = -1 },
	} {
		b := DefaultBudgets()
		mutate(&b)
		if _, err := Evaluate([]Phase{steadyPhase()}, b); err == nil {
			t.Fatalf("budgets %+v accepted", b)
		}
	}
}

func TestReportShowsHotShareAndGrowthPerHour(t *testing.T) {
	p := steadyPhase() // 90 s window
	p.Tables = []TableDelta{{Name: "sage.findings", RowsWritten: 40, Updates: 40,
		HotUpdates: 30, Bytes: 3 * mib, BytesGrowth: mib / 4, Relations: 1}}
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{p}, nil)
	// 0.25 MiB in 90 s is 10 MiB per hour; 30 of 40 updates are HOT.
	const row = "| steady | sage.findings | 1 | 0 | 0 | 0 | 0 | 40 | 40 | 75 | 3.0 | 10.00 |"
	if !strings.Contains(md, row) {
		t.Fatalf("table row missing:\n%s", md)
	}
	if !strings.Contains(md, string(GateHotUpdates)) {
		t.Fatalf("gate F budget missing:\n%s", md)
	}
}
