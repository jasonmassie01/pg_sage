package plan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Ladder tiers a prediction may come from (lab-03's lowest tiers).
const (
	TierHypoPG  = "hypopg"
	TierSession = "session"
	TierClone   = "clone"
	TierCanary  = "canary"
)

// ErrNoTier is a change no available ladder tier can test; the hypothesis
// stays advice.
var ErrNoTier = errors.New("no ladder tier can test this change")

// Proposal is what the ladder tests: a rendered statement (Kind is the
// template, ActionType its contract), the table it touches and the
// statements to plan, or the setting and value for a session test.
type Proposal struct {
	Kind       string
	ActionType string
	Table      string
	SQL        string
	Setting    string
	Value      string
	Queries    []optimizer.QueryInfo
}

// Measured is a ladder prediction: the tier and the predicted improvement
// in percent.
type Measured struct {
	Tier           string
	ImprovementPct float64
}

// Ladder predicts a proposal's effect at the lowest tier it has.
type Ladder interface {
	Predict(ctx context.Context, p Proposal) (Measured, error)
}

// hypoLadder is the what-if tier on the monitored database: HypoPG plans
// the statements with the index as a hypothetical one, read-only, in one
// owning session (optimizer.HypoPG.Validate). It tests indexes only.
type hypoLadder struct{ h *optimizer.HypoPG }

// NewHypoPGLadder is the HypoPG tier on pool; logFn gets HypoPG's
// availability-probe warnings.
func NewHypoPGLadder(pool *pgxpool.Pool, logFn func(string, string, ...any)) Ladder {
	return hypoLadder{h: optimizer.NewHypoPG(pool, logFn)}
}

// Predict measures a create_index_concurrently proposal. Every other kind
// has no tier here (ErrNoTier); an unavailable HypoPG is ErrNoTier too.
func (l hypoLadder) Predict(ctx context.Context, p Proposal) (Measured, error) {
	if p.ActionType != "create_index_concurrently" {
		return Measured{}, fmt.Errorf("%w: %s has no what-if test (HypoPG measures "+
			"indexes only)", ErrNoTier, p.Kind)
	}
	if !l.h.IsAvailable(ctx) {
		return Measured{}, fmt.Errorf("%w: hypopg is not installed on this database",
			ErrNoTier)
	}
	res, err := l.h.Validate(ctx, optimizer.Recommendation{Table: p.Table, DDL: p.SQL},
		p.Queries)
	if err != nil {
		return Measured{}, fmt.Errorf("hypopg: what-if of %s: %w", p.SQL, err)
	}
	if res.Measured == 0 {
		return Measured{}, fmt.Errorf("hypopg: none of the %d statements of %s could be "+
			"planned", len(p.Queries), p.Table)
	}
	return Measured{Tier: TierHypoPG, ImprovementPct: res.Improvement}, nil
}

// sourceOf is a tier's plan-01 expected source. A session-settings test
// is a planner what-if like HypoPG's.
func sourceOf(tier string) changeplan.ExpectedSource {
	switch tier {
	case TierHypoPG, TierSession, "plan_only":
		return changeplan.SourceHypoPG
	case TierClone:
		return changeplan.SourceClone
	case TierCanary, "fleet":
		return changeplan.SourceCanary
	}
	return changeplan.SourceNone
}
