package modelgate

import (
	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/internal/review/scorecard"
)

// EvidenceFor is model's evidence over the newest t.MinReviews reviews that
// count: done, on a real database (not a fixture), with a finished model
// pass on model and no other model. reviews are newest first (Build's
// order).
func EvidenceFor(reviews []scorecard.Review, model string, t Thresholds) Evidence {
	m := modellift.NormalizeModel(model)
	e := Evidence{Model: m, Reviews: modellift.Proportion{N: t.MinReviews}}
	if m == "" || t.MinReviews < 1 {
		return e
	}
	var mc, gc scorecard.Counts
	only := 0
	for _, r := range reviews {
		if e.Reviews.K >= t.MinReviews {
			break
		}
		if !counts(r.Run, m) {
			continue
		}
		e.Reviews.K++
		mc = sum(mc, r.ByOrigin[scorecard.OriginModel])
		gc = sum(gc, r.ByOrigin[scorecard.OriginGenerator])
		only += r.ModelOnlyAccepted
	}
	e.ModelImproved = modellift.Proportion{K: mc.Improved, N: mc.Decided}
	e.GeneratorImproved = modellift.Proportion{K: gc.Improved, N: gc.Decided}
	e.ModelReverts = modellift.Proportion{K: mc.Reverted, N: mc.Decided}
	e.ModelOnlyAccepted = modellift.Proportion{K: only, N: mc.Accepted}
	return e
}

// counts reports whether run counts toward model's gate.
func counts(run scorecard.Run, model string) bool {
	return run.Status == scorecard.StatusDone && !run.Fixture && len(run.Models) == 1 &&
		run.Models[0] == model
}

func sum(a, b scorecard.Counts) scorecard.Counts {
	return scorecard.Counts{Proposed: a.Proposed + b.Proposed,
		Accepted: a.Accepted + b.Accepted, Improved: a.Improved + b.Improved,
		NoChange: a.NoChange + b.NoChange, Inconclusive: a.Inconclusive + b.Inconclusive,
		Regressed: a.Regressed + b.Regressed, Reverted: a.Reverted + b.Reverted,
		Unverifiable: a.Unverifiable + b.Unverifiable, Pending: a.Pending + b.Pending,
		Decided: a.Decided + b.Decided}
}
