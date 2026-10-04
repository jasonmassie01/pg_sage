package earned

import (
	"strings"
	"testing"
	"time"
)

// Roadmap 1.4 (shadow mode): scored shadow decisions count toward a
// self-initiated class's promotion with the family's rules, labelled
// "shadow". L2 (a one-click approval per action) may be earned from
// shadow evidence alone; L3 (unattended) needs at least
// MinRealSuccessesL3 real verified successes, and shadow successes fill
// at most the rest of the L3 bar. The success rate counts every decided
// outcome, real and shadow.

func shadowEvidence(real, shadow, uncredited, shadowUncredited int) Evidence {
	ev := selfEvidence(FamilyTuning, ClassIndexCreate, real+shadow, uncredited, 60*24*time.Hour,
		8*24*time.Hour, 31*24*time.Hour)
	ev.Record.ShadowSuccesses = shadow
	ev.Record.Uncredited = uncredited + shadowUncredited
	ev.Record.ShadowUncredited = shadowUncredited
	return ev
}

func TestShadowEvidenceAloneCanEarnL2(t *testing.T) {
	th := DefaultThresholds()
	a := Assess(th, L2, shadowEvidence(0, 3, 0, 0))
	if !a.Met {
		t.Fatalf("3 shadow successes at L2: %+v", a.Checks)
	}
	c, _ := checkByName(a, "class_successes")
	if !strings.Contains(c.Observed, "3 shadow") || !strings.Contains(c.Observed, "0 real") {
		t.Fatalf("observed %q must show the shadow vs real split", c.Observed)
	}
	if _, ok := checkByName(a, "class_real_successes"); ok {
		t.Fatal("L2 must not require real outcomes")
	}
	a = Assess(th, L2, shadowEvidence(0, 2, 0, 0))
	if c, _ := checkByName(a, "class_successes"); a.Met || c.Met {
		t.Fatalf("2 shadow successes met L2: %+v", a.Checks)
	}
}

func TestL3NeedsRealVerifiedOutcomes(t *testing.T) {
	th := DefaultThresholds()
	if MinRealSuccessesL3 != 3 {
		t.Fatalf("MinRealSuccessesL3 = %d, want 3 (documented minimum)", MinRealSuccessesL3)
	}
	for _, tc := range []struct {
		name               string
		real, shadow       int
		met, realMet       bool
		countedObservedSub string
	}{
		{"all real", 10, 0, true, true, "10"},
		{"3 real + 7 shadow: exactly the bar", 3, 7, true, true, "10 (3 real, 7 shadow)"},
		{"3 real + 50 shadow: shadow capped", 3, 50, true, true, "10 (3 real, 7 shadow"},
		{"2 real + 50 shadow: too few real", 2, 50, false, false, "9 (2 real, 7 shadow"},
		{"3 real + 6 shadow: one short", 3, 6, false, true, "9 (3 real, 6 shadow)"},
		{"0 real + 100 shadow", 0, 100, false, false, "7 (0 real, 7 shadow"},
	} {
		a := Assess(th, L3, shadowEvidence(tc.real, tc.shadow, 0, 0))
		real, _ := checkByName(a, "class_real_successes")
		succ, _ := checkByName(a, "class_successes")
		if a.Met != tc.met || real.Met != tc.realMet {
			t.Errorf("%s: met=%v real=%+v, want met=%v real=%v", tc.name, a.Met, real,
				tc.met, tc.realMet)
		}
		if !strings.HasPrefix(succ.Observed, tc.countedObservedSub) {
			t.Errorf("%s: successes observed %q, want prefix %q", tc.name, succ.Observed,
				tc.countedObservedSub)
		}
		if !tc.realMet && !strings.Contains(real.How, "real") {
			t.Errorf("%s: real check gives no instruction: %+v", tc.name, real)
		}
	}
}

func TestShadowCapFollowsALoweredBar(t *testing.T) {
	th := DefaultThresholds()
	th.ClassMinSuccessesL3 = 2 // fast elevation
	if got := minRealSuccesses(th); got != 2 {
		t.Fatalf("min real with a bar of 2 = %d, want 2", got)
	}
	if !Assess(th, L3, shadowEvidence(2, 5, 0, 0)).Met {
		t.Fatal("2 real successes meet a lowered bar of 2")
	}
	if Assess(th, L3, shadowEvidence(1, 5, 0, 0)).Met {
		t.Fatal("shadow evidence filled a lowered bar that has no room for it")
	}
	if shadowCap(th, L3) != 0 || shadowCap(DefaultThresholds(), L3) != 7 ||
		shadowCap(DefaultThresholds(), L2) != -1 {
		t.Fatalf("caps: lowered %d, default L3 %d, L2 %d", shadowCap(th, L3),
			shadowCap(DefaultThresholds(), L3), shadowCap(DefaultThresholds(), L2))
	}
}

func TestSuccessRateCountsShadowOutcomes(t *testing.T) {
	th := DefaultThresholds()
	// 10 successes (3 real, 7 shadow) of 13 decided: 77% < 80%.
	a := Assess(th, L3, shadowEvidence(3, 7, 0, 3))
	rate, _ := checkByName(a, "class_success_rate")
	if a.Met || rate.Met || !strings.Contains(rate.Observed, "10/13") {
		t.Fatalf("rate with shadow neutrals: %+v", rate)
	}
	// Uncapped shadow successes count in the rate: 3 real + 40 shadow of 46.
	a = Assess(th, L3, shadowEvidence(3, 40, 0, 3))
	if rate, _ := checkByName(a, "class_success_rate"); !rate.Met ||
		!strings.Contains(rate.Observed, "43/46") {
		t.Fatalf("rate with many shadow successes: %+v", rate)
	}
}

func TestDemeritNameKnowsShadowIncorrect(t *testing.T) {
	if demeritName(CauseShadowIncorrect) != CauseShadowIncorrect {
		t.Fatalf("demerit name of %q = %q", CauseShadowIncorrect,
			demeritName(CauseShadowIncorrect))
	}
}
