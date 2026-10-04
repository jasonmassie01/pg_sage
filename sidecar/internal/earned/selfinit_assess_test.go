package earned

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The self-initiated promotion bar (roadmap 1.2): the observation floor
// (the old trust ramp), verified successes since the last demerit and,
// for L3, the success rate. Bench and shadow reviews are incident
// evidence only.

func selfEvidence(f Family, c ActionClass, successes, uncredited int, observed,
	floorL2, floorL3 time.Duration) Evidence {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return Evidence{Family: f, Class: c, At: at,
		Record: &ClassRecord{Successes: successes, Uncredited: uncredited,
			Improved: successes},
		Floor: &FloorStatus{Known: true, Start: at.Add(-observed), Observed: observed,
			RequiredL2: floorL2, RequiredL3: floorL3}}
}

func checkByName(a Assessment, name string) (Check, bool) {
	for _, c := range a.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// Self-initiated promotion bar: the observation floor (the old trust
// ramp) and verified successes since the last demerit; L3 also needs the
// success rate. Bench and shadow reviews are incident evidence only.
func TestAssessSelfInitiatedL2(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassIndexCreate, 3, 0, 9*24*time.Hour,
		8*24*time.Hour, 31*24*time.Hour)
	a := Assess(th, L2, ev)
	if !a.Met {
		t.Fatalf("L2 with floor and 3 successes not met: %+v", a.Checks)
	}
	for _, name := range []string{"bench_present", "shadow_volume", "live_l2_recoveries"} {
		if _, ok := checkByName(a, name); ok {
			t.Fatalf("self-initiated L2 holds incident check %s", name)
		}
	}
	ev.Record.Successes = 2
	a = Assess(th, L2, ev)
	c, ok := checkByName(a, "class_successes")
	if a.Met || !ok || c.Met || c.Observed != "2" || !strings.Contains(c.How, "1 more") {
		t.Fatalf("2 successes: met=%v check=%+v", a.Met, c)
	}
}

func TestAssessSelfInitiatedFloorHasAnETA(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyHygiene, ClassVacuum, 5, 0, 2*24*time.Hour,
		8*24*time.Hour, 8*24*time.Hour)
	a := Assess(th, L2, ev)
	c, ok := checkByName(a, "observation_floor")
	if a.Met || !ok || c.Met {
		t.Fatalf("floor not elapsed but met: %+v", a.Checks)
	}
	wantETA := ev.Floor.Start.Add(8 * 24 * time.Hour)
	if c.ETA == nil || !c.ETA.Equal(wantETA) {
		t.Fatalf("floor ETA = %v, want %v", c.ETA, wantETA)
	}
	if !strings.Contains(c.How, "trust.ramp_safe_hours") {
		t.Fatalf("floor guidance does not name the setting: %q", c.How)
	}
	ev.Floor = &FloorStatus{}
	a = Assess(th, L2, ev)
	if c, _ := checkByName(a, "observation_floor"); a.Met || c.Met {
		t.Fatal("an unknown ramp start met the floor (must fail closed)")
	}
}

func TestAssessSelfInitiatedL3(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassIndexCreate, 10, 2, 40*24*time.Hour,
		8*24*time.Hour, 31*24*time.Hour)
	if a := Assess(th, L3, ev); !a.Met {
		t.Fatalf("L3 with 10 successes, rate 10/12, floor elapsed not met: %+v", a.Checks)
	}
	ev.Record.Uncredited = 3 // 10/13 = 76.9% < 80%
	a := Assess(th, L3, ev)
	if c, ok := checkByName(a, "class_success_rate"); a.Met || !ok || c.Met {
		t.Fatalf("rate 10/13 met L3: %+v", a.Checks)
	}
	ev.Record.Uncredited = 0
	ev.Floor.Observed, ev.Floor.Start = 20*24*time.Hour, ev.At.Add(-20*24*time.Hour)
	a = Assess(th, L3, ev)
	if c, _ := checkByName(a, "observation_floor"); a.Met || c.Met {
		t.Fatalf("L3 met before the moderate floor: %+v", a.Checks)
	}
	if !strings.Contains(fmtChecks(a), "observation_floor") {
		t.Fatal("L3 assessment lacks the floor check")
	}
}

func fmtChecks(a Assessment) string {
	var names []string
	for _, c := range a.Checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// Boundaries: exactly the minimum is enough, one less is not; exactly
// the floor is enough.
func TestAssessSelfInitiatedBoundaries(t *testing.T) {
	th := DefaultThresholds()
	floor := 8 * 24 * time.Hour
	ev := selfEvidence(FamilyHygiene, ClassAnalyze, th.ClassMinSuccessesL2, 0, floor,
		floor, floor)
	if !Assess(th, L2, ev).Met {
		t.Fatal("exactly the minimum successes at exactly the floor is not met")
	}
	ev.Floor.Observed, ev.Floor.Start = floor-time.Second, ev.At.Add(-(floor - time.Second))
	if Assess(th, L2, ev).Met {
		t.Fatal("one second short of the floor met L2")
	}
	ev = selfEvidence(FamilyHygiene, ClassAnalyze, th.ClassMinSuccessesL3, 0, floor,
		floor, floor)
	ev.Record.Uncredited = 0
	if !Assess(th, L3, ev).Met {
		t.Fatal("exactly the L3 minimum is not met")
	}
	ev.Record.Successes--
	if Assess(th, L3, ev).Met {
		t.Fatal("one success short met L3")
	}
}

func TestSupportedLevelSelfInitiated(t *testing.T) {
	th := DefaultThresholds()
	ev := selfEvidence(FamilyTuning, ClassQueryHint, 0, 0, 0, time.Hour, time.Hour)
	if got := SupportedLevel(th, ev); got != L1 {
		t.Fatalf("no evidence supports %v, want L1", got)
	}
	ev = selfEvidence(FamilyTuning, ClassQueryHint, 12, 0, 2*time.Hour, time.Hour,
		time.Hour)
	if got := SupportedLevel(th, ev); got != L3 {
		t.Fatalf("full evidence supports %v, want L3", got)
	}
	ev = selfEvidence(FamilyHygiene, ClassRetention, 50, 0, 900*time.Hour, time.Hour,
		time.Hour)
	if a := Assess(th, L2, ev); a.Met {
		t.Fatal("an irreversible class met L2")
	}
}

func TestThresholdsNormalizeClassBar(t *testing.T) {
	th := Thresholds{ClassMinSuccessesL2: -1, ClassMinSuccessesL3: 0,
		ClassMinSuccessRate: 7}.Normalized()
	def := DefaultThresholds()
	if th.ClassMinSuccessesL2 != def.ClassMinSuccessesL2 ||
		th.ClassMinSuccessesL3 != def.ClassMinSuccessesL3 ||
		th.ClassMinSuccessRate != def.ClassMinSuccessRate {
		t.Fatalf("normalized = %+v", th)
	}
	if def.ClassMinSuccessesL2 != 3 || def.ClassMinSuccessesL3 != 10 ||
		def.ClassMinSuccessRate != 0.8 {
		t.Fatalf("defaults = %d/%d/%v", def.ClassMinSuccessesL2, def.ClassMinSuccessesL3,
			def.ClassMinSuccessRate)
	}
}

// The floor of a level follows the class's tier and reversibility.
func TestRampFloorFor(t *testing.T) {
	r := RampFloor{Start: time.Now(), Safe: 2 * time.Hour, Moderate: 5 * time.Hour}
	cases := []struct {
		target Level
		class  ActionClass
		want   time.Duration
	}{
		{L2, ClassIndexCreate, 2 * time.Hour},
		{L3, ClassIndexCreate, 5 * time.Hour},
		{L3, ClassVacuum, 2 * time.Hour},
		{L3, ClassAnalyze, 2 * time.Hour},
		{L2, ClassRetention, policy.SpecSafeRampAge},
		{L3, ClassRetention, policy.SpecModerateRampAge},
	}
	for _, c := range cases {
		if got := r.For(c.target, c.class); got != c.want {
			t.Errorf("For(%v, %s) = %v, want %v", c.target, c.class, got, c.want)
		}
	}
	zero := RampFloor{Start: time.Now()}
	if got := zero.For(L3, ClassIndexCreate); got != policy.SpecModerateRampAge {
		t.Fatalf("unset ramp takes %v, want the spec", got)
	}
}
