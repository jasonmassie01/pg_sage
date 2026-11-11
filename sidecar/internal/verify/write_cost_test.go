package verify

import (
	"math"
	"strings"
	"testing"
	"time"
)

// executor-03b: the write cost of a verified index is measured on its own
// table. summarizeWriteCycles turns collector cycles into attributable
// intervals; judgeWrites compares two windows against write_impact_pct.
// No concurrent-access tests here: both are pure functions of their inputs.

func ptr64(v int64) *int64 { return &v }

var cycleBase = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// cyc is one collector cycle under statistics epoch e1: the cluster WAL
// position, the table's written tuples and the database's.
func cyc(i int, lsn, table, database int64) writeCycle {
	return writeCycle{At: cycleBase.Add(time.Duration(i) * time.Minute), LSN: ptr64(lsn),
		Epoch: "e1", TableTuples: ptr64(table), DatabaseTuples: ptr64(database)}
}

func TestSummarizeWriteCyclesMeasuresTableDominatedIntervals(t *testing.T) {
	got := summarizeWriteCycles([]writeCycle{
		cyc(0, 0, 0, 0), cyc(1, 100_000, 1000, 1000), cyc(2, 200_000, 2000, 2000),
		cyc(3, 330_000, 3000, 3000),
	})
	want := WriteCost{Intervals: 3, Tuples: 3000, WALBytes: 330_000, Seconds: 180,
		BytesPerTuple: 100}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestSummarizeWriteCyclesTakesTheMedianInterval(t *testing.T) {
	// Per-tuple costs 100, 150, 120, 130: the median is 125, robust to the
	// one 150 burst (another database writing the cluster's WAL).
	got := summarizeWriteCycles([]writeCycle{
		cyc(0, 0, 0, 0), cyc(1, 100_000, 1000, 1000), cyc(2, 250_000, 2000, 2000),
		cyc(3, 370_000, 3000, 3000), cyc(4, 500_000, 4000, 4000),
	})
	if got.Intervals != 4 || got.BytesPerTuple != 125 || got.WALBytes != 500_000 {
		t.Fatalf("summary = %+v, want 4 intervals with a median of 125 bytes", got)
	}
}

// The table must write at least writeDominance of the database's tuples
// (0.9 is attributable, just under is not) and at least
// minIntervalTuples of its own.
func TestSummarizeWriteCyclesDominanceAndVolumeBoundaries(t *testing.T) {
	cases := map[string]struct {
		table, database int64
		attributable    bool
	}{
		"exactly 90% of the database": {900, 1000, true},
		"just under 90%":              {899, 1000, false},
		"another table dominates":     {200, 20_200, false},
		"exactly the minimum volume":  {minIntervalTuples, minIntervalTuples, true},
		"one tuple under the minimum": {minIntervalTuples - 1, minIntervalTuples - 1, false},
		"an idle interval":            {0, 0, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := summarizeWriteCycles([]writeCycle{cyc(0, 1000, 5, 50),
				cyc(1, 51_000, 5+tc.table, 50+tc.database)})
			if tc.attributable && (got.Intervals != 1 || got.Unattributed != 0 ||
				got.Tuples != tc.table || got.WALBytes != 50_000) {
				t.Fatalf("summary = %+v, want one attributable interval", got)
			}
			if !tc.attributable && (got.Intervals != 0 || got.Unattributed != 1 ||
				got.Tuples != 0 || got.BytesPerTuple != 0) {
				t.Fatalf("summary = %+v, want one unattributed interval", got)
			}
		})
	}
}

// An interval whose readings cannot be compared is unknown, never guessed:
// a missing WAL position (a standby, a failed read), a statistics reset
// (the epoch moved, or a counter fell under the same epoch), a legacy row
// without an epoch, a table missing from the snapshot, or the WAL position
// going back (a failover to a server behind).
func TestSummarizeWriteCyclesRefusesIncomparableIntervals(t *testing.T) {
	good := cyc(0, 10_000, 1000, 1000)
	cases := map[string]func(c *writeCycle){
		"no WAL position":        func(c *writeCycle) { c.LSN = nil },
		"WAL position went back": func(c *writeCycle) { c.LSN = ptr64(5000) },
		"epoch moved":            func(c *writeCycle) { c.Epoch = "e2" },
		"no epoch":               func(c *writeCycle) { c.Epoch = "" },
		"table missing":          func(c *writeCycle) { c.TableTuples = nil },
		"no tables snapshot":     func(c *writeCycle) { c.DatabaseTuples = nil },
		"table counter fell":     func(c *writeCycle) { c.TableTuples = ptr64(10) },
		"database counter fell": func(c *writeCycle) {
			c.TableTuples, c.DatabaseTuples = ptr64(1500), ptr64(900)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			next := cyc(1, 110_000, 2000, 2000)
			mutate(&next)
			got := summarizeWriteCycles([]writeCycle{good, next})
			if got != (WriteCost{Unknown: 1}) {
				t.Fatalf("summary = %+v, want one unknown interval", got)
			}
		})
	}
}

func TestSummarizeWriteCyclesEmptyInputs(t *testing.T) {
	if got := summarizeWriteCycles(nil); got != (WriteCost{}) {
		t.Fatalf("nil cycles = %+v, want zero", got)
	}
	if got := summarizeWriteCycles([]writeCycle{cyc(0, 1, 2, 3)}); got != (WriteCost{}) {
		t.Fatalf("one cycle = %+v, want zero (no interval)", got)
	}
}

func measuredCost(bytesPerTuple float64) WriteCost {
	return WriteCost{Intervals: 6, Tuples: 6000, WALBytes: int64(bytesPerTuple * 6000),
		Seconds: 3600, BytesPerTuple: bytesPerTuple}
}

func TestJudgeWritesThresholdBoundary(t *testing.T) {
	cases := map[string]struct {
		after   float64
		outcome string
	}{
		"exactly write_impact_pct": {120, writeWithinThreshold},
		"just above it":            {120.000001, writeRegressed},
		"cost fell":                {80, writeWithinThreshold},
		"unchanged":                {100, writeWithinThreshold},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := judgeWrites(20, measuredCost(100), measuredCost(tc.after))
			if got.Outcome != tc.outcome || !got.Measured {
				t.Fatalf("judgement = %+v, want %s", got, tc.outcome)
			}
			if want := (tc.after - 100); math.Abs(got.DeltaPct-want) > 1e-6 {
				t.Fatalf("delta = %v, want %v", got.DeltaPct, want)
			}
		})
	}
}

// Too little attributable evidence on either side is insufficient: no
// delta and never a regression, whatever the numbers say.
func TestJudgeWritesInsufficientEvidence(t *testing.T) {
	few := measuredCost(500)
	few.Intervals = minWriteIntervals - 1
	thin := measuredCost(500)
	thin.Tuples = minWriteTuples - 1
	cases := map[string]struct{ before, after WriteCost }{
		"no samples at all":            {WriteCost{}, WriteCost{}},
		"no pre-action samples":        {WriteCost{}, measuredCost(500)},
		"no post-action samples":       {measuredCost(100), WriteCost{}},
		"too few post-action interval": {measuredCost(100), few},
		"too few pre-action intervals": {few, measuredCost(500)},
		"too few post-action tuples":   {measuredCost(100), thin},
		"zero pre-action cost":         {measuredCost(0), measuredCost(500)},
		"negative pre-action cost":     {measuredCost(-1), measuredCost(500)},
		"unattributed only": {WriteCost{Unattributed: 12},
			WriteCost{Unattributed: 12, Unknown: 3}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := judgeWrites(20, tc.before, tc.after)
			if got.Outcome != writeInsufficient || got.Measured || got.DeltaPct != 0 ||
				got.Reason == "" {
				t.Fatalf("judgement = %+v, want insufficient with a reason", got)
			}
		})
	}
}

// A zero threshold (never configured; the engine defaults it to 20) still
// needs a strict rise to regress.
func TestJudgeWritesZeroThreshold(t *testing.T) {
	if got := judgeWrites(0, measuredCost(100), measuredCost(100)); got.Outcome !=
		writeWithinThreshold {
		t.Fatalf("equal cost at 0%% = %+v, want within_threshold", got)
	}
	if got := judgeWrites(0, measuredCost(100), measuredCost(101)); got.Outcome !=
		writeRegressed {
		t.Fatalf("1%% rise at 0%% = %+v, want regressed", got)
	}
}

// Seconds sums the attributable intervals only: an interval another table
// dominated or an incomparable one adds no duration.
func TestSummarizeWriteCyclesSumsAttributableSeconds(t *testing.T) {
	reset, after := cyc(3, 330_000, 3000, 3000), cyc(5, 530_000, 5000, 5000)
	reset.Epoch, after.Epoch = "e2", "e2"
	got := summarizeWriteCycles([]writeCycle{
		cyc(0, 0, 0, 0), cyc(1, 100_000, 1000, 1000), cyc(2, 200_000, 1000, 9000),
		reset, after,
	})
	if got.Intervals != 2 || got.Unattributed != 1 || got.Unknown != 1 ||
		got.Seconds != 180 || got.Tuples != 3000 {
		t.Fatalf("summary = %+v, want 2 attributable intervals over 180 s", got)
	}
}

// executor-03b review: part of the WAL is paid per unit of time (checkpoint
// full-page images, the collector's writes), so a table that writes more
// slowly after the build pays more per tuple with no index cost at all. A
// post-action rate of at least 100/(100+pct) of the pre-action one is
// compared; a slower one is insufficient, never a regression.
func TestJudgeWritesRateBoundary(t *testing.T) {
	cases := map[string]struct {
		afterSeconds float64
		outcome      string
	}{
		"same rate":                     {3600, writeRegressed},
		"faster after":                  {1800, writeRegressed},
		"exactly 100/120 of the rate":   {4320, writeRegressed},
		"just under 100/120 of it":      {4320.01, writeInsufficient},
		"a tenth of the rate":           {36_000, writeInsufficient},
		"post-action without duration":  {0, writeInsufficient},
		"negative post-action duration": {-1, writeInsufficient},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			after := measuredCost(300)
			after.Seconds = tc.afterSeconds
			got := judgeWrites(20, measuredCost(100), after)
			if got.Outcome != tc.outcome {
				t.Fatalf("judgement = %+v, want %s", got, tc.outcome)
			}
			if tc.outcome == writeInsufficient && (got.Measured || got.DeltaPct != 0 ||
				got.Reason == "") {
				t.Fatalf("judgement = %+v, want unmeasured with a reason", got)
			}
		})
	}
	before := measuredCost(100)
	before.Seconds = 0
	if got := judgeWrites(20, before, measuredCost(300)); got.Outcome != writeInsufficient ||
		!strings.Contains(got.Reason, "duration") {
		t.Fatalf("pre-action without duration = %+v, want insufficient", got)
	}
	slow := measuredCost(300)
	slow.Seconds = 36_000
	if got := judgeWrites(20, measuredCost(100), slow); !strings.Contains(got.Reason,
		"0.2 tuples/s after the action and 1.7 before") {
		t.Fatalf("reason = %q, want both rates", got.Reason)
	}
}
