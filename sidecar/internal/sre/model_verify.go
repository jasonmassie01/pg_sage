package sre

import (
	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The verifier pass (AI-SRE-SPEC §5, DiagGuard-style). Before an
// investigation concludes, the accepted review is rechecked against the
// final diagnosis and the evidence re-read from the store: the ranking
// must still order exactly the open hypotheses, and every claim must
// cite evidence that is still stored, still matches its hash, and still
// grounds the claim's numbers. When the graph is conclusive and the
// model ranks another hypothesis first, the graph wins: the disagreement
// is reported and nothing the model said is kept.

type verdict struct {
	review    modelReview
	rejected  []*ModelRejection
	disagreed bool
	graphRoot string
	modelRoot string
}

func verifyReview(r modelReview, d causal.Diagnosis, stored []Evidence) verdict {
	scope := newReviewScope(d, stored, false)
	v := verdict{review: r}
	if rej := scope.checkRanking(r.Ranking); rej != nil {
		v.review.Ranking = nil
		v.rejected = append(v.rejected, reject(RejectVerifier, "ranking: %s", rej.Detail))
	} else if d.Conclusive && d.Root != nil && len(r.Ranking) > 0 &&
		r.Ranking[0] != string(d.Root.Node) {
		return verdict{disagreed: true, graphRoot: string(d.Root.Node),
			modelRoot: r.Ranking[0]}
	}
	if rej := verifyClaims(r, scope); rej != nil {
		v.review.Claims = nil
		v.rejected = append(v.rejected, rej)
	}
	return v
}

// verifyClaims rechecks every claim against the stored evidence.
func verifyClaims(r modelReview, scope reviewScope) *ModelRejection {
	if len(r.Claims) == 0 {
		return nil
	}
	texts := EvidenceCatalog{}
	for i, c := range r.Claims {
		for _, alias := range c.EvidenceIDs {
			id, ok := r.aliases[alias]
			if !ok {
				return reject(RejectVerifier, "claim %d cites %s, which was never "+
					"in scope", i+1, alias)
			}
			e, ok := scope.evidence.byID(id)
			switch {
			case !ok:
				return reject(RejectVerifier, "claim %d cites %s, which is no longer "+
					"stored", i+1, alias)
			case e.stale:
				return reject(RejectVerifier, "claim %d cites %s, whose stored evidence "+
					"no longer matches its hash", i+1, alias)
			}
			texts[alias] = e.text
		}
	}
	if err := ValidateClaims(r.Claims, texts); err != nil {
		return reject(RejectVerifier, "claims: %v", err)
	}
	return nil
}
