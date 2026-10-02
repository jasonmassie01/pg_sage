package earned

import (
	"math"
	"testing"
	"time"
)

// Fast elevation: the promotion bar is configurable (sre.autonomy.
// promotion), but a zero or nonsensical threshold never means "skip the
// check" (it takes the spec value), and L4 stays unreachable and every
// class stays under its reversibility cap however low the bar is set.

// fastestThresholds is the lowest bar the config allows.
func fastestThresholds() Thresholds {
	th := DefaultThresholds()
	th.ShadowDuration, th.ShadowMinReviewed, th.ShadowMinAccepted = time.Hour, 3, 0.5
	th.MinTop1, th.MinPrecision, th.MinSafePass, th.GameDayMinSafePass = 0.5, 0.5, 0.5, 0.5
	th.MinL2Recoveries = 1
	return th
}

func TestNormalizedKeepsValidThresholds(t *testing.T) {
	if got := DefaultThresholds().Normalized(); got != DefaultThresholds() {
		t.Fatalf("spec thresholds changed by Normalized: %+v", got)
	}
	if got := fastestThresholds().Normalized(); got != fastestThresholds() {
		t.Fatalf("fastest valid thresholds changed by Normalized: %+v", got)
	}
}

func TestNormalizedReplacesZeroAndInvalidWithTheSpec(t *testing.T) {
	spec := DefaultThresholds()
	if got := (Thresholds{}).Normalized(); got != spec {
		t.Fatalf("zero thresholds = %+v, want the spec %+v", got, spec)
	}
	cases := map[string]struct {
		mutate func(*Thresholds)
		check  func(Thresholds) bool
	}{
		"zero shadow": {func(th *Thresholds) { th.ShadowDuration = 0 },
			func(th Thresholds) bool { return th.ShadowDuration == spec.ShadowDuration }},
		"negative shadow": {func(th *Thresholds) { th.ShadowDuration = -time.Hour },
			func(th Thresholds) bool { return th.ShadowDuration == spec.ShadowDuration }},
		"zero recoveries": {func(th *Thresholds) { th.MinL2Recoveries = 0 },
			func(th Thresholds) bool { return th.MinL2Recoveries == spec.MinL2Recoveries }},
		"zero reviewed": {func(th *Thresholds) { th.ShadowMinReviewed = 0 },
			func(th Thresholds) bool { return th.ShadowMinReviewed == spec.ShadowMinReviewed }},
		"zero acceptance": {func(th *Thresholds) { th.ShadowMinAccepted = 0 },
			func(th Thresholds) bool { return th.ShadowMinAccepted == spec.ShadowMinAccepted }},
		"acceptance above 1": {func(th *Thresholds) { th.ShadowMinAccepted = 95 },
			func(th Thresholds) bool { return th.ShadowMinAccepted == spec.ShadowMinAccepted }},
		"NaN top-1": {func(th *Thresholds) { th.MinTop1 = math.NaN() },
			func(th Thresholds) bool { return th.MinTop1 == spec.MinTop1 }},
		"negative precision": {func(th *Thresholds) { th.MinPrecision = -1 },
			func(th Thresholds) bool { return th.MinPrecision == spec.MinPrecision }},
		"zero safe pass": {func(th *Thresholds) { th.MinSafePass = 0 },
			func(th Thresholds) bool { return th.MinSafePass == spec.MinSafePass }},
		"zero game day": {func(th *Thresholds) { th.GameDayMinSafePass = 0 },
			func(th Thresholds) bool { return th.GameDayMinSafePass == spec.GameDayMinSafePass }},
		"zero bench age": {func(th *Thresholds) { th.BenchMaxAge = 0 },
			func(th Thresholds) bool { return th.BenchMaxAge == spec.BenchMaxAge }},
		"zero top-1 n": {func(th *Thresholds) { th.MinTop1N = 0 },
			func(th Thresholds) bool { return th.MinTop1N == spec.MinTop1N }},
		"zero safe-pass n": {func(th *Thresholds) { th.MinSafePassN = 0 },
			func(th Thresholds) bool { return th.MinSafePassN == spec.MinSafePassN }},
	}
	for name, c := range cases {
		th := fastestThresholds()
		c.mutate(&th)
		got := th.Normalized()
		if !c.check(got) {
			t.Errorf("%s: normalized = %+v", name, got)
		}
		if got.MinL2Recoveries == 0 || got.ShadowDuration <= 0 {
			t.Errorf("%s: a zero threshold survived: %+v", name, got)
		}
	}
}

// fastEvidence just meets the fastest bar: a 1-hour shadow of 3 packets.
func fastEvidence() Evidence {
	ev := strongEvidence()
	ev.Shadow = Shadow{Reviewed: 3, Accepted: 3, FirstReviewAt: assessNow.Add(-time.Hour)}
	ev.Live = Live{VerifiedL2: 1}
	return ev
}

func TestFastThresholdsElevateInHoursButNotBelowTheBar(t *testing.T) {
	th := fastestThresholds()
	ev := fastEvidence()
	if got := SupportedLevel(th, ev); got != L3 {
		t.Fatalf("fast evidence under the fast bar supports %v, want L3", got)
	}
	if got := SupportedLevel(DefaultThresholds(), ev); got != L1 {
		t.Fatalf("fast evidence under the spec bar supports %v, want L1", got)
	}
	short := fastEvidence()
	short.Shadow.FirstReviewAt = assessNow.Add(-59 * time.Minute)
	if a := Assess(th, L2, short); a.Met || checkNamed(t, a, "shadow_duration").Met {
		t.Fatalf("a 59-minute shadow met a 1-hour bar: %+v", a.Checks)
	}
	none := fastEvidence()
	none.Live.VerifiedL2 = 0
	if a := Assess(th, L3, none); a.Met || checkNamed(t, a, "live_l2_recoveries").Met {
		t.Fatalf("L3 met without a live recovery: %+v", a.Checks)
	}
	if got := checkNamed(t, Assess(th, L2, ev), "shadow_duration").Required; got !=
		">= 1h0m0s of shadow reviews" {
		t.Fatalf("shadow requirement reads %q", got)
	}
}

// Invariants under the fastest bar: L4 is never met, harm and safety
// violations still block, and evidence alone never exceeds L3.
func TestFastThresholdsKeepTheInvariants(t *testing.T) {
	th := fastestThresholds()
	ev := fastEvidence()
	if a := Assess(th, L4, ev); a.Met {
		t.Fatalf("L4 met under the fastest bar: %+v", a)
	}
	harmful := fastEvidence()
	harmful.Live.HarmfulPair = 1
	if got := SupportedLevel(th, harmful); got != L2 {
		t.Fatalf("a harmful pair supports %v under the fast bar, want L2", got)
	}
	violated := fastEvidence()
	violated.FamilyViolations = 1
	if got := SupportedLevel(th, violated); got != L1 {
		t.Fatalf("a family safety violation supports %v, want L1", got)
	}
	forbidden := fastEvidence()
	forbidden.Bench.Cells[0].Forbidden = 1
	if got := SupportedLevel(th, forbidden); got != L1 {
		t.Fatalf("a forbidden bench action supports %v, want L1", got)
	}
	for _, spec := range Classes() {
		if spec.Reversibility == Irreversible && spec.Cap > L1 {
			t.Fatalf("irreversible class %s capped at %v", spec.Class, spec.Cap)
		}
	}
}
