package perfgate

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// Gate H: the product's own meter (selfcost, which feeds sage_self_cost,
// the self-budget and /metrics) must agree with the DB time the gate
// measured, and must normalize to the collector period actually observed.

var meterEpoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// knownCost is one known reading of the meter: ms per cycle of cycleSecs.
func knownCost(i int, ms, cycleSecs float64) selfcost.Cost {
	return selfcost.Cost{Known: true, DBTimeKnown: true, Database: "perfgate",
		At: meterEpoch.Add(time.Duration(i+1) * 15 * time.Second), WindowSeconds: 15,
		CycleSeconds: cycleSecs, DBTimeMsPerCycle: ms}
}

// meteredPhase is a steady phase of 6 cycles of 15 s whose statements cost
// gateMs per cycle, with one known meter reading per value in meter.
func meteredPhase(gateMs float64, meter ...float64) Phase {
	p := Phase{Name: "steady", Steady: true, Window: 90 * time.Second, Cycles: 6,
		CollectorPeriod: 15 * time.Second}
	p.Statements = []Statement{{QueryID: 1, Query: "/* pg_sage */ SELECT 1",
		Calls: 6000, TotalMs: gateMs * 6, MeanMs: gateMs / 1000, MaxMs: 1}}
	for i, ms := range meter {
		p.Meter = append(p.Meter, knownCost(i, ms, 15))
	}
	return p
}

func gateOffenders(got []Offender, g Gate) []Offender {
	var out []Offender
	for _, o := range got {
		if o.Gate == g {
			out = append(out, o)
		}
	}
	return out
}

func TestMeterParityHappyPath(t *testing.T) {
	got := meterOffenders(meteredPhase(100, 101))
	if len(got) != 0 {
		t.Fatalf("meter 101 vs gate 100 ms per cycle: offenders = %+v", got)
	}
	// The mean of the known readings is charged, not each one.
	got = meterOffenders(meteredPhase(100, 80, 120, 103))
	if len(got) != 0 {
		t.Fatalf("meter mean 101 vs gate 100: offenders = %+v", got)
	}
}

func TestMeterParityBoundaries(t *testing.T) {
	cases := []struct {
		name          string
		gate, meter   float64
		wantOffending bool
	}{
		{"10.0% over passes", 100, 110, false},
		{"10.0% under passes", 100, 90, false},
		{"10.1% over fails", 100, 110.1, true},
		{"10.1% under fails", 100, 89.9, true},
		{"3 vs 0.5 ms is under the 5 ms floor", 0.5, 3, false},
		{"exactly the 5 ms floor passes", 0.5, 5.5, false},
		{"past the 5 ms floor fails", 0.5, 5.6, true},
		{"meter reads zero while the gate measured 60 ms", 60, 0, true},
		{"both zero", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := meterOffenders(meteredPhase(c.gate, c.meter))
			if !c.wantOffending {
				if len(got) != 0 {
					t.Fatalf("offenders = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Gate != GateMeterParity {
				t.Fatalf("offenders = %+v, want one %s", got, GateMeterParity)
			}
			o := got[0]
			if math.Abs(o.Measured-c.meter) > 1e-9 || math.Abs(o.Budget-c.gate) > 1e-9 {
				t.Fatalf("measured %v budget %v, want meter %v gate %v", o.Measured,
					o.Budget, c.meter, c.gate)
			}
			if o.Phase != "steady" || !strings.Contains(o.Detail, "1 known") {
				t.Fatalf("offender lacks phase or sample count: %+v", o)
			}
		})
	}
}

func TestMeterUnknownIsAnOffenderNotASkip(t *testing.T) {
	unknownOnly := meteredPhase(100)
	unknownOnly.Meter = []selfcost.Cost{
		{At: meterEpoch, CycleSeconds: 15},                               // first sample, no window
		{Known: true, At: meterEpoch.Add(time.Minute), CycleSeconds: 15}, // no pg_stat_statements
	}
	empty := meteredPhase(100)
	empty.Meter = []selfcost.Cost{}
	for name, p := range map[string]Phase{"nil": meteredPhase(100), "empty": empty,
		"only unknown readings": unknownOnly} {
		t.Run(name, func(t *testing.T) {
			got := gateOffenders(meterOffenders(p), GateMeterParity)
			if len(got) != 1 || got[0].Subject != "meter unknown" {
				t.Fatalf("offenders = %+v, want one %q", got, "meter unknown")
			}
			if got[0].Budget != 100 || got[0].Measured != 0 {
				t.Fatalf("meter unknown: measured %v budget %v", got[0].Measured, got[0].Budget)
			}
			want := fmt.Sprintf("among %d readings", len(p.Meter))
			if !strings.Contains(got[0].Detail, want) {
				t.Fatalf("detail %q lacks %q", got[0].Detail, want)
			}
		})
	}
}

func TestMeterUnknownReadingsAreNotAveraged(t *testing.T) {
	p := meteredPhase(100, 100)
	p.Meter = append(p.Meter,
		selfcost.Cost{At: meterEpoch.Add(time.Hour), CycleSeconds: 15},
		selfcost.Cost{Known: true, At: meterEpoch.Add(2 * time.Hour), CycleSeconds: 15})
	if got := meterOffenders(p); len(got) != 0 {
		t.Fatalf("unknown readings (0 ms) pulled the mean down: %+v", got)
	}
}

func TestMeterZeroCycles(t *testing.T) {
	p := meteredPhase(100, 100)
	p.Cycles = 0
	if _, err := Evaluate([]Phase{p}, DefaultBudgets()); err == nil ||
		!strings.Contains(err.Error(), "0 cycles") {
		t.Fatalf("evaluate with 0 cycles: err = %v", err)
	}
	got := meterOffenders(p)
	if len(got) != 1 || got[0].Gate != GateMeterParity ||
		math.IsInf(got[0].Budget, 0) || math.IsNaN(got[0].Budget) {
		t.Fatalf("0 cycles: offenders = %+v, want one finite %s", got, GateMeterParity)
	}
}

func TestMeterCycleSecondsAgainstObservedPeriod(t *testing.T) {
	cases := []struct {
		name          string
		cycle         float64
		period        time.Duration
		wantOffending bool
	}{
		{"60 s configured vs 30 s observed (footprint-02)", 60, 30 * time.Second, true},
		{"equal", 30, 30 * time.Second, false},
		{"10.0% long passes", 33, 30 * time.Second, false},
		{"10.0% short passes", 27, 30 * time.Second, false},
		{"over 10% long fails", 33.1, 30 * time.Second, true},
		{"over 10% short fails", 26.9, 30 * time.Second, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := meteredPhase(100)
			p.CollectorPeriod = c.period
			p.Meter = []selfcost.Cost{knownCost(0, 100, c.period.Seconds()),
				knownCost(1, 100, c.cycle)}
			got := gateOffenders(meterOffenders(p), GateMeterCycle)
			if !c.wantOffending {
				if len(got) != 0 {
					t.Fatalf("offenders = %+v, want none", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("no %s offender for cycle %v s vs %s", GateMeterCycle, c.cycle,
					c.period)
			}
		})
	}
}

func TestMeterCycleReportsWorstReading(t *testing.T) {
	p := meteredPhase(100)
	p.CollectorPeriod = 30 * time.Second
	p.Meter = []selfcost.Cost{knownCost(0, 100, 45), knownCost(1, 100, 60),
		knownCost(2, 100, 30)}
	got := gateOffenders(meterOffenders(p), GateMeterCycle)
	if len(got) != 1 || got[0].Measured != 60 || got[0].Budget != 30 {
		t.Fatalf("offenders = %+v, want one at 60 s against 30 s", got)
	}
	if !strings.Contains(got[0].Detail, "2 of 3") {
		t.Fatalf("detail %q does not count the off readings", got[0].Detail)
	}
}

func TestMeterCollectorPeriodUnknown(t *testing.T) {
	p := meteredPhase(100, 100)
	p.CollectorPeriod = 0
	got := gateOffenders(meterOffenders(p), GateMeterCycle)
	if len(got) != 1 || got[0].Subject != "collector period unknown" {
		t.Fatalf("offenders = %+v, want one %q", got, "collector period unknown")
	}
}

func TestMeterInvalidReadingsFail(t *testing.T) {
	bad := map[string]selfcost.Cost{
		"NaN DB time":         knownCost(5, math.NaN(), 15),
		"negative DB time":    knownCost(5, -1, 15),
		"infinite DB time":    knownCost(5, math.Inf(1), 15),
		"NaN cycle":           knownCost(5, 100, math.NaN()),
		"negative cycle":      knownCost(5, 100, -15),
		"zero cycle on known": knownCost(5, 100, 0),
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			p := meteredPhase(100, 100)
			p.Meter = append(p.Meter, c)
			got := meterOffenders(p)
			var invalid []Offender
			for _, o := range got {
				if o.Subject == "invalid meter reading" {
					invalid = append(invalid, o)
				}
			}
			if len(invalid) != 1 || invalid[0].Gate != GateMeterParity {
				t.Fatalf("offenders = %+v, want one invalid meter reading", got)
			}
			if len(gateOffenders(got, GateMeterParity)) != 1 {
				t.Fatalf("the invalid reading leaked into the mean: %+v", got)
			}
		})
	}
}

func TestMeterUnmeteredPhaseIsReportedNotCharged(t *testing.T) {
	p := meteredPhase(100)
	p.Unmetered = "the history store's DB time is not on the monitored database's meter"
	if got := meterOffenders(p); len(got) != 0 {
		t.Fatalf("unmetered phase charged: %+v", got)
	}
}

func TestEvaluateChargesMeterOnSteadyPhasesOnly(t *testing.T) {
	warm := meteredPhase(100)
	warm.Name, warm.Steady, warm.Cycles = "warmup", false, 0
	steady := meteredPhase(100, 200)
	steady.CPUKnown = true
	steady.ProcessCPU = time.Duration(DefaultBudgets().SidecarCPUMsPerCycle*6+600) *
		time.Millisecond
	got, err := Evaluate([]Phase{warm, steady}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	meter := gateOffenders(got, GateMeterParity)
	if len(meter) != 1 || meter[0].Phase != "steady" {
		t.Fatalf("meter offenders = %+v, want one in the steady phase", meter)
	}
	if got[len(got)-1].Gate != GateMeterParity || got[0].Gate != GateSidecarCPU {
		t.Fatalf("gate H must rank after gate G: %+v", got)
	}
}

func TestHoldsStatement(t *testing.T) {
	sql := "/* pg_sage */ SELECT count(*)::float8\nFROM pg_catalog.pg_class c"
	stmts := []Statement{{Query: "SELECT 1"},
		{Query: "/* pg_sage */ SELECT count(*)::float8 FROM   pg_catalog.pg_class c"}}
	if !HoldsStatement(stmts, sql) {
		t.Fatal("whitespace-different text of the same statement not found")
	}
	if HoldsStatement(stmts[:1], sql) {
		t.Fatal("found a statement that is not there")
	}
	if HoldsStatement(nil, sql) || HoldsStatement(stmts, "") || HoldsStatement(stmts, " \n") {
		t.Fatal("nil statements or an empty text must not match")
	}
}

// The sidecar's pool moves the tag after the first keyword on the wire
// (selfmonitor), so pg_stat_statements holds "SELECT /* pg_sage */ ..."
// for a source text "/* pg_sage */ SELECT ..."; PostgreSQL 18 drops a
// leading comment altogether. Both are the same statement.
func TestHoldsStatementWherePgStatStatementsMovedTheTag(t *testing.T) {
	sql := "/* pg_sage */ SELECT count(*)::float8\nFROM pg_catalog.pg_class c\n" +
		"LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid"
	cases := map[string]string{
		"tag after keyword": "SELECT /* pg_sage */ count(*)::float8 FROM pg_catalog.pg_class c " +
			"LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid",
		"component tag": "SELECT /* pg_sage selfconfig */ count(*)::float8 FROM " +
			"pg_catalog.pg_class c LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid",
		"no tag (PG18)": "SELECT count(*)::float8 FROM pg_catalog.pg_class c " +
			"LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid",
	}
	for name, stored := range cases {
		if !HoldsStatement([]Statement{{Query: stored}}, sql) {
			t.Errorf("%s: %q not recognised as %q", name, stored, sql)
		}
	}
	other := "SELECT /* pg_sage */ count(*)::float8 FROM pg_catalog.pg_class c"
	if HoldsStatement([]Statement{{Query: other}}, sql) {
		t.Errorf("a shorter statement %q matched the full catalog scan", other)
	}
	if HoldsStatement([]Statement{{Query: "SELECT 1"}}, "/* pg_sage */") {
		t.Error("a tag-only text must not match every statement")
	}
}

func TestUntaggedStatementsTopFive(t *testing.T) {
	var stmts []Statement
	for i := 1; i <= 7; i++ {
		stmts = append(stmts, Statement{QueryID: int64(i), Query: "SELECT untagged",
			TotalMs: float64(i)})
	}
	stmts = append(stmts, Statement{QueryID: 99, Query: "/* pg_sage */ SELECT 1",
		TotalMs: 1000})
	got := UntaggedStatements(stmts, 5)
	if len(got) != 5 {
		t.Fatalf("got %d untagged statements, want 5", len(got))
	}
	for i, s := range got {
		if s.QueryID != int64(7-i) {
			t.Fatalf("untagged[%d] = queryid %d, want %d (costliest first)", i, s.QueryID,
				7-i)
		}
	}
	if len(UntaggedStatements(nil, 5)) != 0 || len(UntaggedStatements(stmts, 0)) != 0 {
		t.Fatal("nil statements or n = 0 must give none")
	}
}
