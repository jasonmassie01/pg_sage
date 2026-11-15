package verify

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// lab-01c up-front unverifiable: before a step runs, the targets' expected
// calls within HardMax (per hour, the median of the same hour over the last
// 4 weeks) below min_samples, or fewer than 3 active hours, mean the step
// cannot be judged; no history at all is "unknown", which never blocks.

// pfFrom is when the step would run: a Wednesday 10:00 UTC.
var pfFrom = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// hourly gives id calls at each of the hours (offsets from pfFrom) in each
// of the weeks 1..weeks back.
func hourly(id int64, calls int64, weeks int, offsets ...time.Duration) Series {
	out := Series{}
	for k := 1; k <= weeks; k++ {
		for _, off := range offsets {
			at := pfFrom.Add(off).Truncate(time.Hour).Add(-time.Duration(k) * week)
			out[id] = append(out[id], Bucket{Start: at, Calls: calls, TotalMs: 1,
				Blocks: 1, Rows: 1})
		}
	}
	return out
}

func pfInput(targets []int64, h Series) PreflightInput {
	return PreflightInput{Targets: targets, Hourly: h, From: pfFrom, HardMax: 72 * time.Hour,
		MinSamples: 30}
}

func mustPreflight(t *testing.T, in PreflightInput) Preflight {
	t.Helper()
	p, err := PreflightVerifiable(in)
	if err != nil {
		t.Fatalf("PreflightVerifiable: %v", err)
	}
	return p
}

// CHECK-08: a target that runs once a week (Sunday 03:00, outside the next
// 72 h from a Wednesday) is unverifiable before the step, and the reason is
// the card's: "cannot be verified within 72 h: the targets run N times a
// week".
func TestV2_WeeklyTargetUnverifiableUpFront(t *testing.T) {
	// Sunday 03:00 is 4 days 17 h after Wednesday 10:00.
	weekly := hourly(7, 500, 4, 4*24*time.Hour+17*time.Hour)
	p := mustPreflight(t, pfInput([]int64{7}, weekly))
	if p.Status != PreflightStatusUnverifiable {
		t.Fatalf("weekly target: %+v, want unverifiable", p)
	}
	if p.ExpectedCalls != 0 || p.ActiveHours != 0 || p.WeeklyCalls != 500 {
		t.Fatalf("expected %.0f calls in %d active hours (%.0f a week), want 0, 0, 500",
			p.ExpectedCalls, p.ActiveHours, p.WeeklyCalls)
	}
	want := "cannot be verified within 72 h: the targets run 500 times a week"
	if !strings.Contains(p.Reason, want) {
		t.Fatalf("reason %q, want it to contain %q", p.Reason, want)
	}
	if !p.Blocks() {
		t.Fatal("an unverifiable step must need approval")
	}
	// v1 pin: the v1 watch has no up-front check; it extends at its first
	// look and is reverted only at HardMax (72 h later).
	e := &Engine{options: DefaultOptions()}
	state := WatchState{Window: 2 * time.Hour, ExecutedAt: pfFrom,
		Criterion: Criterion{Window: 2 * time.Hour, HardMax: 72 * time.Hour}}
	if v := e.insufficientVerdict(state, 0); v.Status != "extended" || v.Revert {
		t.Fatalf("v1 pin: first look %+v, want extended (v1 cannot know up front)", v)
	}
}

// The weekly job inside the window is still too few active hours.
func TestPreflight_WeeklyJobInsideTheWindowHasOneActiveHour(t *testing.T) {
	p := mustPreflight(t, pfInput([]int64{7}, hourly(7, 5000, 4, 20*time.Hour)))
	if p.Status != PreflightStatusUnverifiable || p.ActiveHours != 1 || p.ExpectedCalls != 5000 {
		t.Fatalf("one busy hour: %+v, want unverifiable with 1 active hour, 5000 calls", p)
	}
	if !strings.Contains(p.Reason, "1 active hour") {
		t.Fatalf("reason %q must name the active hours", p.Reason)
	}
}

func TestPreflight_BusyTargetsAreVerifiable(t *testing.T) {
	var offsets []time.Duration
	for h := 0; h < 72; h++ {
		offsets = append(offsets, time.Duration(h)*time.Hour)
	}
	h := hourly(1, 10, 4, offsets...)
	for id, b := range hourly(2, 5, 4, offsets...) {
		h[id] = b
	}
	p := mustPreflight(t, pfInput([]int64{1, 2}, h))
	if p.Status != PreflightStatusVerifiable || p.ExpectedCalls != 72*15 || p.ActiveHours != 72 {
		t.Fatalf("busy targets: %+v, want verifiable, 1080 calls, 72 active hours", p)
	}
	if p.WeeksSeen != 4 || p.Blocks() {
		t.Fatalf("weeks seen %d (blocks %v), want 4 and no block", p.WeeksSeen, p.Blocks())
	}
}

// Boundary: expected calls exactly min_samples is verifiable, one fewer is
// not; 3 active hours are enough, 2 are not.
func TestPreflight_MinSamplesAndActiveHoursBoundaries(t *testing.T) {
	three := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour}
	exact := hourly(1, 10, 4, three...) // 3 hours x 10 = 30 = min_samples
	fewer := hourly(1, 10, 4, three...)
	for i := range fewer[1] {
		if fewer[1][i].Start.Hour() == pfFrom.Add(time.Hour).Hour() {
			fewer[1][i].Calls = 9 // 9 + 10 + 10 = 29
		}
	}
	two := hourly(1, 20, 4, three[:2]...)
	for _, tc := range []struct {
		name  string
		h     Series
		want  string
		calls float64
	}{
		{"exactly min_samples", exact, PreflightStatusVerifiable, 30},
		{"one fewer", fewer, PreflightStatusUnverifiable, 29},
		{"two active hours", two, PreflightStatusUnverifiable, 40},
	} {
		p := mustPreflight(t, pfInput([]int64{1}, tc.h))
		if p.Status != tc.want || p.ExpectedCalls != tc.calls {
			t.Fatalf("%s: %+v, want %s with %.0f calls", tc.name, p, tc.want, tc.calls)
		}
	}
}

// The median, not the mean: one busy outlier week does not make a quiet
// target verifiable.
func TestPreflight_MedianIgnoresOneOutlierWeek(t *testing.T) {
	h := hourly(1, 1, 4, time.Hour, 2*time.Hour, 3*time.Hour)
	for i := range h[1] {
		if h[1][i].Start.After(pfFrom.Add(-week)) {
			h[1][i].Calls = 1000 // the most recent week only
		}
	}
	p := mustPreflight(t, pfInput([]int64{1}, h))
	if p.Status != PreflightStatusUnverifiable || p.ExpectedCalls != 3 {
		t.Fatalf("outlier week: %+v, want unverifiable with 3 expected calls", p)
	}
}

// A new database (no evid-01 rows) is unknown: never inferred either way.
func TestPreflight_NoHistoryIsUnknown(t *testing.T) {
	for name, h := range map[string]Series{"nil": nil, "empty": {},
		"other query": hourly(99, 100, 4, time.Hour)} {
		p := mustPreflight(t, pfInput([]int64{1}, h))
		if p.Status != PreflightStatusUnknown || !strings.Contains(p.Reason, "no hourly history") {
			t.Fatalf("%s: %+v, want unknown naming the missing history", name, p)
		}
		if p.Blocks() {
			t.Fatalf("%s: unknown must not block the step", name)
		}
	}
	p := mustPreflight(t, pfInput(nil, hourly(1, 100, 4, time.Hour)))
	if p.Status != PreflightStatusUnknown || !strings.Contains(p.Reason, "no target") {
		t.Fatalf("no targets: %+v, want unknown naming the missing targets", p)
	}
}

// A target with only one week of history is judged on that week (weeks
// without any row are not counted as zero-call weeks).
func TestPreflight_ShortHistoryUsesTheWeeksSeen(t *testing.T) {
	var offsets []time.Duration
	for h := 0; h < 24; h++ {
		offsets = append(offsets, time.Duration(h)*time.Hour)
	}
	p := mustPreflight(t, pfInput([]int64{1}, hourly(1, 5, 1, offsets...)))
	if p.Status != PreflightStatusVerifiable || p.WeeksSeen != 1 || p.ExpectedCalls != 120 {
		t.Fatalf("one week: %+v, want verifiable from 1 week, 120 calls", p)
	}
}

func TestPreflight_RefusesInvalidInput(t *testing.T) {
	for name, in := range map[string]PreflightInput{
		"zero hard max":    {Targets: []int64{1}, From: pfFrom, MinSamples: 30},
		"zero min samples": {Targets: []int64{1}, From: pfFrom, HardMax: time.Hour},
		"zero from":        {Targets: []int64{1}, HardMax: time.Hour, MinSamples: 30},
	} {
		if _, err := PreflightVerifiable(in); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestPreflight_MalformedBucketsAreIgnored(t *testing.T) {
	h := hourly(1, 10, 4, time.Hour, 2*time.Hour, 3*time.Hour)
	h[1] = append(h[1], Bucket{Start: pfFrom.Add(time.Hour - week), Calls: -50})
	p := mustPreflight(t, pfInput([]int64{1}, h))
	if p.Status != PreflightStatusVerifiable || p.ExpectedCalls != 30 {
		t.Fatalf("a negative bucket changed the count: %+v", p)
	}
}

func TestPreflight_ExemptClasses(t *testing.T) {
	for class, want := range map[string]bool{ClassVacuum: true, ClassAnalyze: true,
		ClassReindex: true, ClassIndexCreate: false, ClassGUC: false, "": false} {
		if got := PreflightExempt(class); got != want {
			t.Fatalf("PreflightExempt(%q) = %v, want %v", class, got, want)
		}
	}
}

func TestPreflight_EvidenceIsJSONSafe(t *testing.T) {
	p := mustPreflight(t, pfInput([]int64{7}, hourly(7, 500, 4, 100*time.Hour)))
	ev := p.Evidence()
	if ev["status"] != PreflightStatusUnverifiable || ev["min_samples"] != 30 ||
		ev["hard_max_hours"] != 72.0 || ev["weekly_calls"] != 500.0 {
		t.Fatalf("evidence %v", ev)
	}
}

func TestPreflight_ConcurrentCallsAgree(t *testing.T) {
	in := pfInput([]int64{7}, hourly(7, 500, 4, time.Hour, 2*time.Hour, 3*time.Hour))
	want := mustPreflight(t, in)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := PreflightVerifiable(in); err != nil ||
				got.ExpectedCalls != want.ExpectedCalls || got.Status != want.Status {
				t.Errorf("concurrent preflight %+v %v, want %+v", got, err, want)
			}
		}()
	}
	wg.Wait()
}
