package earned

import (
	"strings"
	"testing"
	"time"
)

// Phase 1.1 promotion coach: every unmet check carries a plain
// instruction (How) with the counts behind it and, where the rule implies
// one, the time it will be met (ETA). Met checks carry neither.

// No DB, no concurrency: Assess is a pure function of its inputs.

var guideAt = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func fastThresholds() Thresholds {
	th := DefaultThresholds()
	th.ShadowDuration, th.ShadowMinReviewed = 4*time.Hour, 3
	return th
}

func checkNamed(t *testing.T, a Assessment, name string) Check {
	t.Helper()
	for _, c := range a.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %s in %+v", name, a.Checks)
	return Check{}
}

func TestGuidanceShadowChecks(t *testing.T) {
	first := guideAt.Add(-time.Hour)
	ev := Evidence{Family: FamilyLockBlocking, Class: ClassBackendCancel, At: guideAt,
		Shadow: Shadow{Reviewed: 1, Accepted: 1, FirstReviewAt: first}}
	a := Assess(fastThresholds(), L2, ev)
	vol := checkNamed(t, a, "shadow_volume")
	if vol.Met || !strings.Contains(vol.How, "Review 2 more") ||
		!strings.Contains(vol.How, "last 4 h") || vol.ETA != nil {
		t.Fatalf("shadow_volume = %+v", vol)
	}
	dur := checkNamed(t, a, "shadow_duration")
	if dur.Met || dur.ETA == nil || !dur.ETA.Equal(first.Add(4*time.Hour)) ||
		!strings.Contains(dur.How, "3 h") {
		t.Fatalf("shadow_duration = %+v, want ETA first review + 4 h", dur)
	}
	if acc := checkNamed(t, a, "shadow_acceptance"); !acc.Met || acc.How != "" ||
		acc.ETA != nil {
		t.Fatalf("a met check carries guidance: %+v", acc)
	}
}

func TestGuidanceWithoutAnyReview(t *testing.T) {
	ev := Evidence{Family: FamilyLockBlocking, Class: ClassBackendCancel, At: guideAt}
	a := Assess(DefaultThresholds(), L2, ev)
	dur := checkNamed(t, a, "shadow_duration")
	if dur.ETA != nil || !strings.Contains(dur.How, "Accept or reject") ||
		!strings.Contains(dur.How, "30 days") {
		t.Fatalf("shadow_duration without reviews = %+v", dur)
	}
	if vol := checkNamed(t, a, "shadow_volume"); !strings.Contains(vol.How, "Review 20 more") {
		t.Fatalf("shadow_volume = %+v", vol)
	}
}

func TestGuidanceAcceptanceCountsTheReviewsNeeded(t *testing.T) {
	ev := Evidence{Family: FamilyLockBlocking, Class: ClassBackendCancel, At: guideAt,
		Shadow: Shadow{Reviewed: 3, Accepted: 2, FirstReviewAt: guideAt.Add(-5 * time.Hour)}}
	acc := checkNamed(t, Assess(fastThresholds(), L2, ev), "shadow_acceptance")
	// (2+a)/(3+a) >= 0.95 needs a = 17 more accepted reviews.
	if acc.Met || !strings.Contains(acc.How, "17 more accepted") ||
		!strings.Contains(acc.How, "2 of 3") {
		t.Fatalf("shadow_acceptance = %+v", acc)
	}
	// Boundary: 19 of 20 is exactly 95%.
	ev.Shadow = Shadow{Reviewed: 20, Accepted: 19, FirstReviewAt: ev.Shadow.FirstReviewAt}
	if acc = checkNamed(t, Assess(fastThresholds(), L2, ev), "shadow_acceptance"); !acc.Met {
		t.Fatalf("19/20 = 95%% must meet the bar: %+v", acc)
	}
}

func TestGuidanceBenchChecks(t *testing.T) {
	ev := Evidence{Family: FamilyWAL, Class: ClassWALBound, At: guideAt}
	a := Assess(DefaultThresholds(), L2, ev)
	present := checkNamed(t, a, "bench_present")
	if !strings.Contains(present.How, "bench_results_path") ||
		!strings.Contains(present.How, "pload") || !strings.Contains(present.How,
		string(FamilyWAL)) {
		t.Fatalf("bench_present = %+v", present)
	}
	ev.Bench = &EvalRun{ID: "b1", GeneratedAt: guideAt.Add(-40 * 24 * time.Hour),
		Gated: []string{"causal-graph"}, Cells: []Cell{{Arm: "causal-graph",
			Family: string(FamilyWAL), Top1: Metric{K: 5, N: 12}}}}
	a = Assess(DefaultThresholds(), L2, ev)
	if fresh := checkNamed(t, a, "bench_fresh"); fresh.Met ||
		!strings.Contains(fresh.How, "30 days") {
		t.Fatalf("bench_fresh = %+v", fresh)
	}
	if top := checkNamed(t, a, "bench_top1"); top.Met || !strings.Contains(top.How, "5/12") {
		t.Fatalf("bench_top1 = %+v", top)
	}
}

func TestGuidanceSafetyAndLiveChecks(t *testing.T) {
	clears := guideAt.Add(72 * time.Hour)
	ev := Evidence{Family: FamilyWraparound, Class: ClassFreeze, At: guideAt,
		FamilyViolations: 2, ViolationsClearAt: &clears,
		Live: Live{VerifiedL2: 47, HarmfulPair: 1, Unverified: 4}}
	a := Assess(DefaultThresholds(), L3, ev)
	safety := checkNamed(t, a, "no_safety_violations")
	if safety.ETA == nil || !safety.ETA.Equal(clears) || !strings.Contains(safety.How, "2 ") {
		t.Fatalf("no_safety_violations = %+v", safety)
	}
	live := checkNamed(t, a, "live_l2_recoveries")
	if !strings.Contains(live.How, "3 more") || !strings.Contains(live.How, "4 unverified") {
		t.Fatalf("live_l2_recoveries = %+v", live)
	}
	if harm := checkNamed(t, a, "no_harmful_actions"); !strings.Contains(harm.How, "never") {
		t.Fatalf("no_harmful_actions = %+v", harm)
	}
}

func TestGuidanceDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * 24 * time.Hour: "30 days", 24 * time.Hour: "1 day", 4 * time.Hour: "4 h",
		90 * time.Minute: "90 min", 0: "0 min", -time.Hour: "0 min",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	for _, c := range []struct {
		accepted, reviewed int
		rate               float64
		want               int
	}{{2, 3, 0.95, 17}, {19, 20, 0.95, 0}, {0, 0, 0.95, 1}, {3, 4, 1, -1}, {0, 5, 0.5, 5}} {
		if got := acceptedNeeded(c.accepted, c.reviewed, c.rate); got != c.want {
			t.Errorf("acceptedNeeded(%d, %d, %v) = %d, want %d", c.accepted, c.reviewed,
				c.rate, got, c.want)
		}
	}
}
