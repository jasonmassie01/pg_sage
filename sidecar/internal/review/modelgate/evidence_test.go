package modelgate

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/internal/review/scorecard"
)

// No concurrent access test: EvidenceFor is a pure function of its input.

// review is a scorecard review of model with one decided model step
// (improved when improved) and one improved generator step.
func review(id int64, run scorecard.Run, improved bool) scorecard.Review {
	run.ID = id
	m := scorecard.Counts{Proposed: 1, Accepted: 1, Decided: 1}
	if improved {
		m.Improved = 1
	}
	g := scorecard.Counts{Proposed: 1, Accepted: 1, Improved: 1, Decided: 1}
	return scorecard.Review{Run: run, ModelOnlyAccepted: 1,
		ByOrigin: map[string]scorecard.Counts{scorecard.OriginModel: m,
			scorecard.OriginGenerator: g}}
}

func done(models ...string) scorecard.Run {
	return scorecard.Run{Status: scorecard.StatusDone, Models: models}
}

// Only done reviews on a real database whose model pass finished on the
// model, and no other, count; the window stops at min_reviews, newest
// first.
func TestEvidenceFor_CountsOnlyRealDoneReviewsOfTheModel(t *testing.T) {
	fixture := done("m1")
	fixture.Fixture = true
	partial := done("m1")
	partial.Status = scorecard.StatusPartial
	reviews := []scorecard.Review{
		review(1, fixture, false), review(2, partial, false),
		review(3, done("m1", "m2"), false), review(4, done("m2"), false),
		review(5, done(), false), review(6, done("m1"), true),
		review(7, done("m1"), true), review(8, done("m1"), false),
	}
	e := EvidenceFor(reviews, " OpenAI/M1 ", Thresholds{MinReviews: 2, MaxGapPoints: 10,
		MaxRevertPct: 2})
	want := Evidence{Model: "m1", Reviews: modellift.Proportion{K: 2, N: 2},
		ModelImproved:     modellift.Proportion{K: 2, N: 2},
		GeneratorImproved: modellift.Proportion{K: 2, N: 2},
		ModelReverts:      modellift.Proportion{K: 0, N: 2},
		ModelOnlyAccepted: modellift.Proportion{K: 2, N: 2}}
	if e != want {
		t.Fatalf("evidence = %+v, want %+v (reviews 6 and 7 only)", e, want)
	}
}

func TestEvidenceFor_NothingCountsWithoutAModelOrAWindow(t *testing.T) {
	reviews := []scorecard.Review{review(1, done("m1"), true)}
	for name, tc := range map[string]struct {
		model string
		th    Thresholds
	}{"no model": {"  ", DefaultThresholds()},
		"no window": {"m1", Thresholds{MinReviews: 0}}} {
		e := EvidenceFor(reviews, tc.model, tc.th)
		if e.Reviews.K != 0 || e.ModelImproved != (modellift.Proportion{}) {
			t.Fatalf("%s: evidence = %+v, want nothing counted", name, e)
		}
	}
	if e := EvidenceFor(nil, "m1", DefaultThresholds()); e.Reviews !=
		(modellift.Proportion{K: 0, N: 50}) || EvaluateM1(e, DefaultThresholds()).Eligible {
		t.Fatalf("no reviews: evidence = %+v, want 0 of 50 and not eligible", e)
	}
}
