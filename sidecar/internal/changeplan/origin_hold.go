package changeplan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReasonModelOrigin prefixes why a model-proposed step needs its own
// approval (review-03b): until a person grants the model authority
// (review-05a's G-M1, a live sage.review_model_authority row for its model
// id), approving the plan does not cover a step only a model proposed,
// whatever its class's trust.
const ReasonModelOrigin = "model_origin"

// readModelHolds returns, for a model-origin plan, the action steps that
// must be approved on their own, keyed by step id, with why. A step's
// origin comes from sage.plan_step_origin; a step of a model-origin plan
// without an origin row counts as the model's (fail closed). Plans of
// any other origin hold nothing, so their coverage is unchanged.
func readModelHolds(ctx context.Context, q querier, p Plan) (map[int64]string, error) {
	if p.Origin != OriginModel || p.ID <= 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT s.id, COALESCE(o.origin_ref, $2)
		  FROM sage.plan_step s
		  LEFT JOIN sage.plan_step_origin o ON o.step_id = s.id
		 WHERE s.plan_id = $1 AND s.kind = 'action'
		   AND COALESCE(o.origin, 'model') = 'model'
		   AND NOT EXISTS (SELECT 1 FROM sage.review_model_authority a
		                    WHERE a.model = COALESCE(o.origin_ref, $2)
		                      AND a.model <> '' AND a.revoked_at IS NULL)`, p.ID, p.Model)
	if err != nil {
		return nil, fmt.Errorf("read the model-origin holds of plan %d: %w", p.ID, err)
	}
	pairs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (modelHold, error) {
		var h modelHold
		return h, r.Scan(&h.step, &h.model)
	})
	if err != nil {
		return nil, fmt.Errorf("read the model-origin holds of plan %d: %w", p.ID, err)
	}
	out := make(map[int64]string, len(pairs))
	for _, h := range pairs {
		model := h.model
		if model == "" {
			model = "an unnamed model"
		}
		out[h.step] = fmt.Sprintf("%s: proposed by %s, which has no authority to run "+
			"steps on a plan approval (G-M1); approve this step on its own", ReasonModelOrigin,
			model)
	}
	return out, nil
}

type modelHold struct {
	step  int64
	model string
}

// CoverageWithHolds is Coverage where each step id in holds also needs its
// own approval, with the hold as the reason when no class reason applies.
func CoverageWithHolds(p Plan, live *Approval, steps []StepApproval,
	settings SettingContexts, holds map[int64]string) []StepCoverage {
	own := map[int64]bool{}
	for _, sa := range steps {
		if sa.VoidedAt == nil && sa.PlanHash == p.PlanHash {
			own[sa.StepID] = true
		}
	}
	if live != nil && live.PlanHash != p.PlanHash {
		live = nil
	}
	out := make([]StepCoverage, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, stepCoverage(s, live, own[s.ID], settings, holds[s.ID]))
	}
	return out
}
