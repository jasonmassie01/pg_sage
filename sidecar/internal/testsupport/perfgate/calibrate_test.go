package perfgate

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// GitHub's shared runners differ by 10-25% from one night to the next on
// the same statements, so absolute millisecond budgets flapped on runner
// speed alone. The gate times a fixed CPU workload and a fixed SQL
// workload on the runner and scales its timing budgets by the runner's
// time over the reference runner's, both ways: a faster runner is held to
// tighter budgets, so its speed cannot hide a regression, and a slower one
// gets looser budgets, so its slowness is not a failure. Each factor is
// clamped to MinCalibrationFactor-MaxCalibrationFactor.

// factorTolerance is far below the boundary tests' 1e-9 steps and far
// above a ratio's rounding error.
const factorTolerance = 1e-12

func mustCalibration(t *testing.T, cpuMs, dbMs float64) Calibration {
	t.Helper()
	c, err := NewCalibration(cpuMs, dbMs)
	if err != nil {
		t.Fatalf("calibration(%v, %v): %v", cpuMs, dbMs, err)
	}
	return c
}

func near(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

// The review set the clamp: a floor around x0.75, a cap around x1.25.
func TestCalibrationClampIsTheReviewedRange(t *testing.T) {
	if MinCalibrationFactor != 0.75 || MaxCalibrationFactor != 1.25 {
		t.Fatalf("clamp x%v-x%v, want x0.75-x1.25", MinCalibrationFactor,
			MaxCalibrationFactor)
	}
}

func TestCalibrationFactorIsRunnerOverReference(t *testing.T) {
	cases := []struct {
		name            string
		cpu, db         float64
		wantCPU, wantDB float64
	}{
		{"the reference runner", ReferenceCPUMs, ReferenceDBMs, 1, 1},
		{"faster: tighter", ReferenceCPUMs * 0.8, ReferenceDBMs * 0.9, 0.8, 0.9},
		{"slower: looser", ReferenceCPUMs * 1.2, ReferenceDBMs * 1.1, 1.2, 1.1},
		{"each workload its own factor", ReferenceCPUMs * 0.85, ReferenceDBMs * 1.15,
			0.85, 1.15},
	}
	for _, c := range cases {
		got := mustCalibration(t, c.cpu, c.db)
		if !got.Known || got.CPUMs != c.cpu || got.DBMs != c.db ||
			math.Abs(got.CPUFactor-c.wantCPU) > factorTolerance ||
			math.Abs(got.DBFactor-c.wantDB) > factorTolerance {
			t.Fatalf("%s: calibration(%v, %v) = %+v, want factors %v and %v", c.name,
				c.cpu, c.db, got, c.wantCPU, c.wantDB)
		}
	}
}

// Exactly at a bound the factor is the bound, just inside it is the
// measured ratio, just outside it (and far outside) it is the bound.
func TestCalibrationFactorBoundaries(t *testing.T) {
	const step = 1e-9
	cases := []struct{ ratio, want float64 }{
		{MinCalibrationFactor, MinCalibrationFactor},
		{MinCalibrationFactor + step, MinCalibrationFactor + step},
		{MinCalibrationFactor - step, MinCalibrationFactor},
		{0.5, MinCalibrationFactor},
		{1e-6, MinCalibrationFactor},
		{MaxCalibrationFactor, MaxCalibrationFactor},
		{MaxCalibrationFactor - step, MaxCalibrationFactor - step},
		{MaxCalibrationFactor + step, MaxCalibrationFactor},
		{4, MaxCalibrationFactor},
		{1e6, MaxCalibrationFactor},
	}
	for _, c := range cases {
		got := mustCalibration(t, ReferenceCPUMs*c.ratio, ReferenceDBMs*c.ratio)
		if math.Abs(got.CPUFactor-c.want) > factorTolerance ||
			math.Abs(got.DBFactor-c.want) > factorTolerance {
			t.Fatalf("ratio %.10f: factors cpu %.12f, db %.12f; want %.12f", c.ratio,
				got.CPUFactor, got.DBFactor, c.want)
		}
	}
}

// A time that is not a positive, finite number of milliseconds is a broken
// measurement, not a runner's speed: it is rejected, naming the workload,
// and never turned into a factor.
func TestCalibrationRejectsInvalidTimes(t *testing.T) {
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		c, err := NewCalibration(bad, ReferenceDBMs)
		if !errors.Is(err, ErrInvalidCalibration) || c.Known ||
			!strings.Contains(err.Error(), "CPU workload") {
			t.Fatalf("cpu %v: %+v, %v; want ErrInvalidCalibration naming the CPU "+
				"workload", bad, c, err)
		}
		c, err = NewCalibration(ReferenceCPUMs, bad)
		if !errors.Is(err, ErrInvalidCalibration) || c.Known ||
			!strings.Contains(err.Error(), "SQL workload") {
			t.Fatalf("sql %v: %+v, %v; want ErrInvalidCalibration naming the SQL "+
				"workload", bad, c, err)
		}
	}
}

// Every timing budget scales, both ways: the database's by the SQL
// workload's factor, the sidecar's CPU by the CPU workload's, the API
// endpoint by the larger of the two.
func TestCalibratedBudgetsScaleBothWays(t *testing.T) {
	b := DefaultBudgets()
	cases := []struct {
		name               string
		cpu, db, endpoints float64
	}{
		{"slow SQL, fast CPU", 0.8, 1.2, 1.2},
		{"fast SQL, slow CPU", 1.2, 0.8, 1.2},
		{"fast at both", 0.8, 0.9, 0.9},
		{"slow at both", 1.1, 1.2, 1.2},
	}
	for _, c := range cases {
		cal := mustCalibration(t, ReferenceCPUMs*c.cpu, ReferenceDBMs*c.db)
		got := b.Calibrated(cal)
		if !near(got.StatementMeanMs, b.StatementMeanMs*c.db) ||
			!near(got.CycleDBTimeMs, b.CycleDBTimeMs*c.db) ||
			!near(got.CatalogStatementMaxMs, b.CatalogStatementMaxMs*c.db) ||
			!near(got.SidecarCPUMsPerCycle, b.SidecarCPUMsPerCycle*c.cpu) ||
			!near(got.EndpointMaxMs, b.EndpointMaxMs*c.endpoints) ||
			!near(cal.EndpointFactor(), c.endpoints) {
			t.Fatalf("%s: calibrated budgets %+v (endpoint factor %v)", c.name, got,
				cal.EndpointFactor())
		}
		if err := got.validate(); err != nil {
			t.Fatalf("%s: calibrated budgets are invalid: %v", c.name, err)
		}
	}
}

// Counts are not times: row, scan and HOT budgets and the HOT exemptions
// never scale, the budgets Calibrated was called on are left as they were,
// and an unmeasured calibration scales nothing.
func TestCalibratedBudgetsKeepCountsAndTheOriginal(t *testing.T) {
	b := DefaultBudgets()
	got := b.Calibrated(mustCalibration(t, ReferenceCPUMs*1.2, ReferenceDBMs*0.8))
	if got.SeqScanMinRows != b.SeqScanMinRows ||
		got.RowsWrittenPerCycle != b.RowsWrittenPerCycle ||
		got.HotUpdateMinPct != b.HotUpdateMinPct || got.HotMinUpdates != b.HotMinUpdates ||
		!reflect.DeepEqual(got.HotExempt, b.HotExempt) {
		t.Fatalf("count budgets or HOT exemptions changed: %+v", got)
	}
	if !reflect.DeepEqual(b, DefaultBudgets()) {
		t.Fatalf("Calibrated modified the budgets it was called on: %+v", b)
	}
	if same := b.Calibrated(Calibration{}); !reflect.DeepEqual(same, b) {
		t.Fatalf("an unmeasured calibration scaled the budgets: %+v", same)
	}
	b.MeanExempt = nil
	got = b.Calibrated(mustCalibration(t, ReferenceCPUMs, ReferenceDBMs*1.2))
	if got.MeanExempt != nil || !near(got.StatementMeanMs, b.StatementMeanMs*1.2) {
		t.Fatalf("budgets without mean exemptions: %+v, want none and a scaled mean", got)
	}
}

// A mean exemption's ceiling is a database time like the mean budget: it
// scales by the SQL workload's factor, or a slow runner would fail a
// ceiling the plain budget passes and a fast one would hide a regression
// under it.
func TestCalibratedBudgetsScaleMeanExemptionCeilings(t *testing.T) {
	b := DefaultBudgets()
	if len(b.MeanExempt) == 0 {
		t.Fatal("no mean exemptions to scale")
	}
	for _, f := range []float64{0.8, 1.2} {
		got := b.Calibrated(mustCalibration(t, ReferenceCPUMs, ReferenceDBMs*f))
		if len(got.MeanExempt) != len(b.MeanExempt) {
			t.Fatalf("x%v: exemptions %v, want %v", f, got.MeanExempt, b.MeanExempt)
		}
		for tag, ex := range b.MeanExempt {
			g, ok := got.MeanExempt[tag]
			if !ok || g.Reason != ex.Reason || !near(g.CeilingMs, ex.CeilingMs*f) {
				t.Fatalf("x%v: exemption %s = %+v, want ceiling %v and the same reason",
					f, tag, g, ex.CeilingMs*f)
			}
		}
	}
}

// Through the gate: a tagged statement 10% over its shipped ceiling passes
// on a runner 20% slower at SQL; one 10% under it fails on a runner 20%
// faster, against the scaled ceiling.
func TestCalibratedCeilingsDecideExemptStatements(t *testing.T) {
	b := DefaultBudgets()
	ceiling := b.MeanExempt[clusterSizeTag].CeilingMs
	slow := b.Calibrated(mustCalibration(t, ReferenceCPUMs, ReferenceDBMs*1.2))
	fast := b.Calibrated(mustCalibration(t, ReferenceCPUMs, ReferenceDBMs*0.8))
	if got := meanGates(t, b, clusterSize(ceiling*1.1)); len(got) != 1 {
		t.Fatalf("shipped budgets, 10%% over the ceiling: %+v, want one offender", got)
	}
	if got := meanGates(t, slow, clusterSize(ceiling*1.1)); len(got) != 0 {
		t.Fatalf("runner 20%% slower, 10%% over the shipped ceiling: %+v, want none", got)
	}
	if got := meanGates(t, b, clusterSize(ceiling*0.9)); len(got) != 0 {
		t.Fatalf("shipped budgets, 10%% under the ceiling: %+v, want none", got)
	}
	got := meanGates(t, fast, clusterSize(ceiling*0.9))
	if len(got) != 1 || !near(got[0].Budget, ceiling*0.8) {
		t.Fatalf("runner 20%% faster, 10%% under the shipped ceiling: %+v, want one "+
			"offender against a %v ms ceiling", got, ceiling*0.8)
	}
}

// A runner 20% slower passes a statement and a CPU cost 15% over the
// shipped budgets; a runner 20% faster fails both at 90% of them; a 4x
// regression fails on the slowest runner the clamp allows for.
func TestCalibrationDecidesBorderlineOffenders(t *testing.T) {
	phase := func(meanMs, cpuMsPerCycle float64) Phase {
		p := steadyPhase()
		p.Statements = []Statement{{QueryID: 1,
			Query: "SELECT /* pg_sage */ 1 FROM sage.findings", Calls: 6,
			TotalMs: 6 * meanMs, MeanMs: meanMs, MaxMs: meanMs}}
		p.ProcessCPU = time.Duration(6 * cpuMsPerCycle * float64(time.Millisecond))
		p.CPUKnown = true
		return p
	}
	offenders := func(p Phase, b Budgets) int {
		t.Helper()
		got, err := Evaluate([]Phase{p}, b)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		return len(got)
	}
	b := DefaultBudgets()
	over, under := phase(115, 690), phase(90, 540)
	slow := b.Calibrated(mustCalibration(t, ReferenceCPUMs*1.2, ReferenceDBMs*1.2))
	fast := b.Calibrated(mustCalibration(t, ReferenceCPUMs*0.8, ReferenceDBMs*0.8))
	if n := offenders(over, b); n != 2 {
		t.Fatalf("shipped budgets, 15%% over: %d offenders, want the mean and the CPU", n)
	}
	if n := offenders(over, slow); n != 0 {
		t.Fatalf("runner 20%% slower, 15%% over: %d offenders, want none", n)
	}
	if n := offenders(under, b); n != 0 {
		t.Fatalf("shipped budgets, 10%% under: %d offenders, want none", n)
	}
	if n := offenders(under, fast); n != 2 {
		t.Fatalf("runner 20%% faster, 10%% under: %d offenders, want the mean and "+
			"the CPU", n)
	}
	slowest := b.Calibrated(mustCalibration(t, 1e9, 1e9))
	if n := offenders(phase(400, 2400), slowest); n != 2 {
		t.Fatalf("a 4x regression on a very slow runner: %d offenders, want 2", n)
	}
}
