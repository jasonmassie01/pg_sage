package backtest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bench fixtures (lab-02): v2's placebo false-positive rate is at or
// below 5% on every null fixture, v1's is above it on the traffic shift
// (the back-test can fail a verifier), and v2 detects the injected 30%
// drop on at least 80% of the placebos wherever targets run.

// fixtureEnd is the clock of a fixture run: its last day.
var fixtureEnd = FixtureStart.Add(FixtureDays * day)

func fixtureConfig(name string, seed int64) Config {
	return Config{Database: name, Days: DefaultDays, Placebos: DefaultPlacebos, Seed: seed,
		MaxRuntime: time.Hour, Cycle: FixtureCycle, Now: func() time.Time { return fixtureEnd }}
}

var (
	fixtureOnce    sync.Once
	fixtureResults map[string]Result
	fixtureErr     error
)

// benchResults runs every fixture once per test binary (seed 1).
func benchResults(t *testing.T) map[string]Result {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureResults = map[string]Result{}
		for _, name := range FixtureNames {
			m, err := Fixture(name)
			if err != nil {
				fixtureErr = err
				return
			}
			r, err := Run(context.Background(), m, fixtureConfig(name, 1))
			if err != nil {
				fixtureErr = err
				return
			}
			fixtureResults[name] = r
		}
	})
	if fixtureErr != nil {
		t.Fatalf("bench fixtures: %v", fixtureErr)
	}
	return fixtureResults
}

func TestBacktest_NullFixturesV2AtOrBelowFivePercent(t *testing.T) {
	for name, r := range benchResults(t) {
		c := r.V2
		if c.Placebos != DefaultPlacebos {
			t.Errorf("%s: %d placebos, want %d", name, c.Placebos, DefaultPlacebos)
		}
		if c.Usable() < 100 {
			t.Errorf("%s: only %d usable placebos", name, c.Usable())
		}
		if c.FPRate() > GateMaxFPRate {
			t.Errorf("%s: v2 false-positive rate %s above 5%% (%d of %d)", name,
				Pct(c.FPRate()), c.FalsePositives, c.Usable())
		}
		if c.FPWilsonUpper() < c.FPRate() || c.FPWilsonUpper() <= 0 {
			t.Errorf("%s: Wilson upper %v not above the rate %v", name, c.FPWilsonUpper(),
				c.FPRate())
		}
		if r.Verifier != "v2" || r.Database != name || !r.RanAt.Equal(fixtureEnd) {
			t.Errorf("%s: result header %q %q %s", name, r.Verifier, r.Database, r.RanAt)
		}
		if r.Global == 0 || r.Global == c.Placebos {
			t.Errorf("%s: %d global placebos of %d: want both classes", name, r.Global,
				c.Placebos)
		}
	}
}

// Pinned: the back-test must be able to fail. v1 pools time per call by
// the post-period call mix, so a mix shift looks like a change.
func TestBacktest_V1TrafficShiftAboveFivePercent(t *testing.T) {
	r := benchResults(t)[FixtureTrafficShift]
	if r.V1.FPRate() <= GateMaxFPRate {
		t.Fatalf("v1 false-positive rate on %s = %s, want above 5%% (%+v)",
			FixtureTrafficShift, Pct(r.V1.FPRate()), r.V1)
	}
	if ok, reason := Gate("v2", r.V1); ok || !strings.Contains(reason,
		"false-positive rate") {
		t.Fatalf("gate on v1's counts = %v %q, want refused on the false-positive rate",
			ok, reason)
	}
	if r.V2.FalsePositives >= r.V1.FalsePositives {
		t.Fatalf("v2 has %d false positives, v1 %d: v2 should beat v1 here",
			r.V2.FalsePositives, r.V1.FalsePositives)
	}
}

func TestBacktest_InjectedDetectionAtLeastEightyPercent(t *testing.T) {
	for name, r := range benchResults(t) {
		c := r.V2
		if c.Injected != c.Usable() {
			t.Errorf("%s: %d injected, want the %d usable", name, c.Injected, c.Usable())
		}
		if name == FixtureWeeklyOnly {
			continue
		}
		if c.DetectionRate() < GateMinDetection {
			t.Errorf("%s: v2 detection %s below 80%% (%d of %d)", name,
				Pct(c.DetectionRate()), c.Detected, c.Injected)
		}
	}
}

// Weekly-only targets are mostly never seen: the inconclusive share shows
// it (the "too strict" signal) and the detection floor holds v2 back.
func TestBacktest_WeeklyOnlyTargetsAreInconclusiveNotFalse(t *testing.T) {
	r := benchResults(t)[FixtureWeeklyOnly]
	if r.V2.InconclusiveShare() < 0.10 {
		t.Fatalf("inconclusive share %s, want at least 10%% (%+v)",
			Pct(r.V2.InconclusiveShare()), r.V2)
	}
	if r.V2.FalsePositives != 0 {
		t.Fatalf("%d false positives on unseen targets, want 0", r.V2.FalsePositives)
	}
	if ok, reason := r.Gate(); ok || !strings.HasPrefix(reason, "detection ") {
		t.Fatalf("gate = %v %q, want refused on the detection floor", ok, reason)
	}
}

// Deploy breaks (a reset, a Neon or Lakebase suspend and resume): the
// placebos on a break are excluded from the denominator and counted.
func TestBacktest_EpochBreaksAreExcludedAndCounted(t *testing.T) {
	r := benchResults(t)[FixtureDeployBreaks]
	c := r.V2
	if c.Excluded == 0 || c.Excluded == c.Placebos {
		t.Fatalf("excluded %d of %d, want some", c.Excluded, c.Placebos)
	}
	if r.V1.Excluded != c.Excluded {
		t.Fatalf("v1 excluded %d, v2 %d: the same placebos must be left out",
			r.V1.Excluded, c.Excluded)
	}
	if c.FalsePositives+c.Inconclusive > c.Usable() {
		t.Fatalf("counts exceed the usable placebos: %+v", c)
	}
	if !strings.Contains(r.Text(), "excluded on an epoch break") {
		t.Fatalf("report does not state the excluded count:\n%s", r.Text())
	}
	for _, name := range []string{FixtureSeasonalWeek, FixtureTrafficShift} {
		if n := benchResults(t)[name].V2.Excluded; n != 0 {
			t.Errorf("%s has no break but excluded %d", name, n)
		}
	}
}

func TestBacktest_SameSeedSameRow(t *testing.T) {
	m, err := Fixture(FixtureNoisyNeighbour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(FixtureNoisyNeighbour, 1)
	again, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	first := benchResults(t)[FixtureNoisyNeighbour]
	if again != first {
		t.Fatalf("same seed, different result:\n%+v\n%+v", first, again)
	}
	cfg.Seed = 2
	other, err := Run(context.Background(), m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if other.Seed != 2 || other.V2.Placebos != first.V2.Placebos {
		t.Fatalf("seed 2 result %+v", other)
	}
}

func TestFixture_UnknownAndInvalid(t *testing.T) {
	if _, err := Fixture("no-such-fixture"); err == nil ||
		!strings.Contains(err.Error(), "no-such-fixture") {
		t.Fatalf("Fixture(unknown) = %v", err)
	}
	if _, err := FixtureAt(FixtureSeasonalWeek, FixtureStart, 0, FixtureCycle); err == nil {
		t.Fatal("FixtureAt with 0 days accepted")
	}
	if _, err := FixtureAt(FixtureSeasonalWeek, FixtureStart, 1, 0); err == nil {
		t.Fatal("FixtureAt with a zero cycle accepted")
	}
}

// Fixtures are deterministic: two builds hold the same cycles.
func TestFixture_Deterministic(t *testing.T) {
	a, _ := FixtureAt(FixtureTrafficShift, FixtureStart, 2, time.Hour)
	b, _ := FixtureAt(FixtureTrafficShift, FixtureStart, 2, time.Hour)
	ia, ib := a.Intervals(101), b.Intervals(101)
	if len(ia) != 48 || len(ia) != len(ib) {
		t.Fatalf("cycles %d and %d, want 48", len(ia), len(ib))
	}
	for i := range ia {
		if ia[i] != ib[i] {
			t.Fatalf("cycle %d differs: %+v vs %+v", i, ia[i], ib[i])
		}
	}
	var calls int64
	for _, iv := range ia {
		calls += iv.Calls
	}
	if s := a.Stmts[0]; s.QueryID != 101 || s.Calls != calls || s.Tables[0] != "public.t1" {
		t.Fatalf("statement 101 = %+v, want %d calls on public.t1", s, calls)
	}
}
