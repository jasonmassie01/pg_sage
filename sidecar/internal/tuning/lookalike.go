package tuning

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// DetailLookalikePrior is the finding detail key of the fleet's
// look-alike evidence; LookalikeSource labels it.
const (
	DetailLookalikePrior = "lookalike_prior"
	LookalikeSource      = "from look-alike databases"
)

// LookalikePrior is what databases resembling this one observed after
// comparable actions (fleet learning). It is evidence only: it is shown,
// it may add caution, it orders proposals that have no local calibration,
// and it never sets confidence, removes an approval requirement or changes
// trust.
type LookalikePrior struct {
	Databases     int
	MinSimilarity float64
	Match         string // "table_shape" or "action_class"
	Improved      int
	Neutral       int
	Regressed     int
}

// Outcomes is the number of verified outcomes behind the prior.
func (p LookalikePrior) Outcomes() int { return p.Improved + p.Neutral + p.Regressed }

// Cautions reports look-alikes that regressed at least as often as they
// improved.
func (p LookalikePrior) Cautions() bool {
	return p.Regressed > 0 && p.Regressed >= p.Improved
}

// PriorSource answers look-alike priors; nil, nil means no prior.
type PriorSource interface {
	LookalikePrior(ctx context.Context, database, class string,
		tables []string) (*LookalikePrior, error)
}

// applyPriors attaches each admitted proposal's look-alike prior. A
// failure is logged and leaves the proposal as it was.
func (a *Agent) applyPriors(ctx context.Context, judged []Judged) {
	if a.deps.Priors == nil {
		return
	}
	for i := range judged {
		j := &judged[i]
		if j.Finding == nil || j.Class == "" {
			continue
		}
		p, err := a.deps.Priors.LookalikePrior(ctx, a.settings.DatabaseName, j.Class,
			j.Tables)
		if err != nil {
			a.logFn("WARN", "tuning: look-alike prior for %s on %v unavailable, "+
				"proposal kept without it: %v", j.Class, j.Tables, err)
			continue
		}
		if p == nil || p.Outcomes() == 0 {
			continue
		}
		j.Prior = p
		j.Finding.Detail[DetailLookalikePrior] = priorDetail(*p)
	}
}

func priorDetail(p LookalikePrior) map[string]any {
	lo, _ := wilson(p.Improved, p.Outcomes())
	return map[string]any{"source": LookalikeSource, "match": p.Match,
		"databases": p.Databases, "min_similarity": p.MinSimilarity,
		"improved": p.Improved, "neutral": p.Neutral, "regressed": p.Regressed,
		"outcomes": p.Outcomes(),
		"value":    float64(p.Improved) / float64(p.Outcomes()), "wilson_low": lo}
}

// applyPriorCaution requires approval when look-alikes mostly regressed;
// an existing reason (the local calibration's) is kept.
func applyPriorCaution(f *analyzer.Finding, p *LookalikePrior, redirected bool) {
	if f == nil || p == nil || redirected || !p.Cautions() {
		return
	}
	if _, already := f.Detail[analyzer.DetailApprovalRequired]; already {
		return
	}
	f.Detail[analyzer.DetailApprovalRequired] = fmt.Sprintf("look-alike databases "+
		"regressed: %d of %d comparable actions regressed on %d look-alike database(s)",
		p.Regressed, p.Outcomes(), p.Databases)
}

// priorOrder orders proposals without local calibration: the lower
// Wilson bound of the look-alikes' improvements, 0 without a usable prior.
func priorOrder(p *LookalikePrior) float64 {
	if p == nil || p.Outcomes() == 0 || p.Cautions() {
		return 0
	}
	lo, _ := wilson(p.Improved, p.Outcomes())
	return lo
}
