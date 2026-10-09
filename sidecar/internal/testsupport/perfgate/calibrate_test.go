package perfgate

import (
	"context"
	"math"
	"strings"
	"testing"
)

// GitHub's shared runners differ by 10-25% from one night to the next on
// the same statements, so absolute millisecond budgets flapped on runner
// speed alone. The gate times a fixed CPU workload and a fixed SQL
// workload on the runner and scales its timing budgets by how much slower
// the runner is than the reference runner the budgets were set on: never
// tighter than the shipped budgets, at most MaxCalibrationFactor looser.

func TestCalibrationFactorsAreClamped(t *testing.T) {
	cases := []struct {
		cpu, db         float64
		wantCPU, wantDB float64
	}{
		{ReferenceCPUMs, ReferenceDBMs, 1, 1},                 // the reference runner
		{ReferenceCPUMs / 2, ReferenceDBMs / 3, 1, 1},         // faster: never tighter
		{ReferenceCPUMs * 1.2, ReferenceDBMs * 1.1, 1.2, 1.1}, // a slow night
		{ReferenceCPUMs * 3, ReferenceDBMs * 9, MaxCalibrationFactor, MaxCalibrationFactor},
		{0, -1, 1, 1},                   // unknown: no scaling
		{math.NaN(), math.Inf(1), 1, 1}, // nonsense: no scaling
	}
	for _, c := range cases {
		got := NewCalibration(c.cpu, c.db)
		if math.Abs(got.CPUFactor-c.wantCPU) > 1e-9 || math.Abs(got.DBFactor-c.wantDB) > 1e-9 {
			t.Fatalf("calibration(%v, %v) = cpu %v db %v, want %v %v", c.cpu, c.db,
				got.CPUFactor, got.DBFactor, c.wantCPU, c.wantDB)
		}
	}
	if MaxCalibrationFactor <= 1 || MaxCalibrationFactor > 2 {
		t.Fatalf("max factor %v: a slow runner may loosen budgets a little, not hide a "+
			"regression", MaxCalibrationFactor)
	}
}

// Only the timing budgets scale; row, HOT and scan budgets are counts and
// the exemptions are unchanged.
func TestCalibratedBudgetsScaleOnlyTimings(t *testing.T) {
	b := DefaultBudgets()
	c := Calibration{CPUFactor: 1.25, DBFactor: 1.4, Known: true}
	got := b.Calibrated(c)
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if !near(got.StatementMeanMs, b.StatementMeanMs*1.4) ||
		!near(got.CycleDBTimeMs, b.CycleDBTimeMs*1.4) ||
		!near(got.CatalogStatementMaxMs, b.CatalogStatementMaxMs*1.4) ||
		!near(got.EndpointMaxMs, b.EndpointMaxMs*1.4) ||
		!near(got.SidecarCPUMsPerCycle, b.SidecarCPUMsPerCycle*1.25) {
		t.Fatalf("timing budgets not scaled: %+v", got)
	}
	if got.SeqScanMinRows != b.SeqScanMinRows || got.RowsWrittenPerCycle != b.RowsWrittenPerCycle ||
		got.HotUpdateMinPct != b.HotUpdateMinPct || got.HotMinUpdates != b.HotMinUpdates ||
		len(got.HotExempt) != len(b.HotExempt) || len(got.MeanExempt) != len(b.MeanExempt) {
		t.Fatalf("count budgets or exemptions changed: %+v", got)
	}
	if b.StatementMeanMs != DefaultBudgets().StatementMeanMs {
		t.Fatal("Calibrated modified the budgets it was called on")
	}
	if same := b.Calibrated(Calibration{}); same.SidecarCPUMsPerCycle != b.SidecarCPUMsPerCycle {
		t.Fatal("an unknown calibration scaled the budgets")
	}
}

// A runner 20% slower passes a statement 15% over the shipped budget; a
// regression past the clamped factor still fails.
func TestCalibrationDecidesBorderlineOffenders(t *testing.T) {
	p := steadyPhase()
	p.Statements = []Statement{{QueryID: 1, Query: "SELECT /* pg_sage */ 1 FROM sage.findings",
		Calls: 6, TotalMs: 690, MeanMs: 115, MaxMs: 130}}
	p.ProcessCPU, p.CPUKnown = 6*690*1e6, true // 690 ms per cycle
	b := DefaultBudgets()
	if got, _ := Evaluate([]Phase{p}, b); len(got) != 2 {
		t.Fatalf("shipped budgets: %d offenders, want the mean and the CPU", len(got))
	}
	slow := NewCalibration(ReferenceCPUMs*1.2, ReferenceDBMs*1.2)
	if got, _ := Evaluate([]Phase{p}, b.Calibrated(slow)); len(got) != 0 {
		t.Fatalf("20%% slower runner: offenders %+v, want none", got)
	}
	p.Statements[0].MeanMs = 400
	if got, _ := Evaluate([]Phase{p}, b.Calibrated(NewCalibration(1e9, 1e9))); len(got) != 1 {
		t.Fatalf("a 4x regression on a very slow runner: %+v, want it charged", got)
	}
}

func TestCalibrateCPUMeasuresTheFixedWorkload(t *testing.T) {
	ms := CalibrateCPU()
	if ms <= 0 || ms > 60000 {
		t.Fatalf("cpu calibration = %v ms", ms)
	}
}

func TestCalibrateDBMeasuresTheFixedQuery(t *testing.T) {
	pool, ctx := livePool(t)
	ms, err := CalibrateDB(ctx, pool)
	if err != nil || ms <= 0 || ms > 60000 {
		t.Fatalf("db calibration = %v ms, %v", ms, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CalibrateDB(canceled, pool); err == nil ||
		!strings.Contains(err.Error(), "calibrat") {
		t.Fatalf("canceled calibration: %v, want an error naming calibration", err)
	}
	if _, err := CalibrateDB(ctx, nil); err == nil {
		t.Fatal("calibration without a pool returned no error")
	}
}

// The report shows what the runner measured and the budgets it was held to.
func TestReportShowsCalibration(t *testing.T) {
	c := NewCalibration(ReferenceCPUMs*1.3, ReferenceDBMs*1.1)
	b := DefaultBudgets().Calibrated(c)
	md := RenderCalibratedMarkdown(SmallScale(), b, c, []Phase{steadyPhase()}, nil)
	for _, want := range []string{"## Runner calibration", "CPU workload", "SQL workload",
		"x1.30", "x1.10", "780 ms (steady phase)"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
}
