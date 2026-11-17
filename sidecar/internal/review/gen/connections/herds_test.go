package connections

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// apiHerd is 40 api sessions that connected within one minute 30 minutes
// before t0 and 20 more one per minute before them, seen hourly 3 times.
func apiHerd() []Sample {
	starts := append(synced(t0.Add(-30*time.Minute), 40),
		staggered(t0.Add(-2*time.Hour), 20)...)
	return hourly("api", starts)
}

// Happy path: 40 of 60 sessions in one minute across 3 samples is a herd
// citing the three samples' connection hours.
func TestBuild_FortySessionsConnectedInOneMinuteAcrossThreeSamplesAreAHerd(t *testing.T) {
	cands, _ := build(t, plainPack(t), Inputs{Samples: apiHerd()})
	c := only(t, cands, KindRecycleHerd)
	if c.Target != "application:api" || c.ActionType != candidate.ActionAdvice ||
		c.Approval != candidate.ApprovalRequired || c.Generator != Name {
		t.Fatalf("herd %+v", c)
	}
	want := []string{hourID(t0Hour), hourID(t0Hour + 1), hourID(t0Hour + 2)}
	if !slices.Equal(c.EvidenceIDs, want) {
		t.Fatalf("evidence %v, want %v", c.EvidenceIDs, want)
	}
	q := c.Prediction.Quantities
	if q[QuantityHerd] != 1 || q[QuantityHerdSamples] != 3 || q[QuantityHerdSessions] != 40 {
		t.Fatalf("quantities %v, want herd 1, 3 samples, 40 sessions", q)
	}
	if got := q[QuantityHerdSharePct]; got < 66.6 || got > 66.7 {
		t.Fatalf("share %v%%, want 66.7%%", got)
	}
	for _, want := range []string{"40 of 60", "api", "jitter", "never changes"} {
		if !strings.Contains(strings.ToLower(c.Reason), strings.ToLower(want)) {
			t.Errorf("reason lacks %q: %s", want, c.Reason)
		}
	}
	if Herds(cands) != 1 {
		t.Fatalf("Herds = %d, want 1", Herds(cands))
	}
}

// Boundary: the same cohort in only 2 samples is no herd.
func TestBuild_HerdInTwoSamplesIsNotAHerd(t *testing.T) {
	cands, _ := build(t, plainPack(t), Inputs{Samples: apiHerd()[:2]})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 || Herds(cands) != 0 {
		t.Fatalf("%d herds from 2 samples", n)
	}
}

// Staggered sessions, one per minute, are no herd.
func TestBuild_StaggeredSessionsAreNoHerd(t *testing.T) {
	cands, _ := build(t, plainPack(t),
		Inputs{Samples: hourly("api", staggered(t0.Add(-time.Hour), 40))})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 {
		t.Fatalf("%d herds from staggered sessions: %+v", n, cands)
	}
}

// shareSamples is three samples of app with inBucket sessions in one
// minute and the rest one per minute.
func shareSamples(app string, inBucket, total int) []Sample {
	starts := append(synced(t0.Add(-10*time.Minute), inBucket),
		staggered(t0.Add(-time.Hour), total-inBucket)...)
	return hourly(app, starts)
}

// Boundary: 30% of an application's sessions in one minute is not a herd;
// 30.1% is.
func TestBuild_ThirtyPercentIsNotAHerdAndThirtyPointOneIs(t *testing.T) {
	for _, tc := range []struct {
		inBucket int
		herd     bool
	}{{300, false}, {301, true}} {
		cands, _ := build(t, plainPack(t), Inputs{Samples: shareSamples("api", tc.inBucket,
			1000)})
		if got := len(ofKind(cands, KindRecycleHerd)) == 1; got != tc.herd {
			t.Errorf("%d of 1000 in one minute: herd %v, want %v", tc.inBucket, got, tc.herd)
		}
	}
}

// Boundary: an application under HerdMinSessions sessions is never judged.
func TestBuild_SmallApplicationIsNeverAHerd(t *testing.T) {
	for _, tc := range []struct {
		n    int
		herd bool
	}{{HerdMinSessions - 1, false}, {HerdMinSessions, true}} {
		cands, _ := build(t, plainPack(t),
			Inputs{Samples: hourly("api", synced(t0.Add(-time.Hour), tc.n))})
		if got := len(ofKind(cands, KindRecycleHerd)) == 1; got != tc.herd {
			t.Errorf("%d sessions all in one minute: herd %v, want %v", tc.n, got, tc.herd)
		}
	}
}

// withAges replaces each sample's ages with rows.
func withAges(samples []Sample, rows ...collector.WorkloadAppAge) []Sample {
	for i := range samples {
		samples[i].Ages = append(slices.Clone(samples[i].Ages), rows...)
	}
	return samples
}

// Invalid input: a NULL backend_start (a session whose start pg_sage's
// role cannot see) is skipped: it is neither a bucket nor counted in the
// share. Counted as one bucket, 30 of 40 would be a herd.
func TestBuild_NullBackendStartIsSkipped(t *testing.T) {
	samples := withAges(hourly("worker", staggered(t0.Add(-time.Hour), 10)),
		collector.WorkloadAppAge{Application: "worker", Sessions: 30})
	cands, cov := build(t, plainPack(t), Inputs{Samples: samples})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 {
		t.Fatalf("NULL starts made %d herds: %+v", n, cands)
	}
	if !hasNote(cov, "90 session ages were unknown") {
		t.Fatalf("notes %v lack the unknown age count", cov.Notes)
	}
}

// Invalid input: a negative age (clock skew, or a session that connected
// after the read began) is skipped with a count.
func TestBuild_NegativeAgeIsSkippedWithACount(t *testing.T) {
	samples := withAges(hourly("api", staggered(t0.Add(-time.Hour), 10)),
		collector.WorkloadAppAge{Application: "api", Minute: minute(-1), Sessions: 5})
	cands, cov := build(t, plainPack(t), Inputs{Samples: samples})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 {
		t.Fatalf("negative ages made %d herds: %+v", n, cands)
	}
	if !hasNote(cov, "15 session ages were negative") {
		t.Fatalf("notes %v lack the negative age count", cov.Notes)
	}
}

// A truncated age read is judged against the application's group total,
// so a cut list never inflates a share: 40 of 200 is 20%.
func TestBuild_TruncatedAgesUseTheGroupTotal(t *testing.T) {
	samples := hourly("api", synced(t0.Add(-time.Hour), 40))
	for i := range samples {
		samples[i].AgesTruncated, samples[i].Apps["api"] = true, 200
	}
	cands, _ := build(t, plainPack(t), Inputs{Samples: samples})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 {
		t.Fatalf("40 of 200 sessions made %d herds", n)
	}
}

// Samples written before the application ages were collected judge no
// herd and say so.
func TestBuild_SamplesWithoutAgesAreNotedNotJudged(t *testing.T) {
	samples := apiHerd()
	for i := range samples {
		samples[i].Ages = nil
	}
	cands, cov := build(t, plainPack(t), Inputs{Samples: samples})
	if n := len(ofKind(cands, KindRecycleHerd)); n != 0 {
		t.Fatalf("%d herds without ages", n)
	}
	if !hasNote(cov, "3 samples", "no connection ages") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

// Two herding applications are two candidates, in application order.
func TestBuild_EachHerdingApplicationIsOneCandidate(t *testing.T) {
	a, b := apiHerd(), hourly("billing", synced(t0.Add(-5*time.Minute), 12))
	for i := range a {
		a[i].Apps["billing"] = b[i].Apps["billing"]
		a[i].Ages = append(a[i].Ages, b[i].Ages...)
	}
	cands, _ := build(t, plainPack(t), Inputs{Samples: a})
	herds := ofKind(cands, KindRecycleHerd)
	if len(herds) != 2 || herds[0].Target != "application:api" ||
		herds[1].Target != "application:billing" || Herds(cands) != 2 {
		t.Fatalf("herds %+v", herds)
	}
}
