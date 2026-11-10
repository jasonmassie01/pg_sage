package tuner

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/planfresh"
)

// WithPlanSource sets the bounded plain EXPLAIN that replaces a stale
// cached plan and the timeouts of the plan reads (tuning-08). Without it a
// stale plan leaves the candidate without a plan.
func (t *Tuner) WithPlanSource(explain planfresh.ExplainFunc, timeouts catalogread.Timeouts,
) *Tuner {
	t.planExplain, t.planTimeouts = explain, timeouts
	return t
}

func (t *Tuner) planMaxAge() time.Duration { return planfresh.MaxAge(t.cfg.PlanMaxAgeMinutes) }

// readPlan reads queryID's newest fresh plan. Only a statement that has a
// cached plan is planned afresh, so the tuner never EXPLAINs a statement
// it did not plan from before. A failed read leaves it without a plan.
func (t *Tuner) readPlan(ctx context.Context, queryID int64, text string) planfresh.Plan {
	if t.pool == nil {
		return planfresh.Plan{Source: planfresh.SourceNone}
	}
	r := planfresh.Reader{DB: catalogread.New(t.pool, t.planTimeouts), MaxAge: t.planMaxAge(),
		Explain: t.explainOrNone(queryID)}
	p, err := r.Read(ctx, queryID, text)
	if err != nil {
		t.logFn("WARN", "tuner: no plan for queryid=%d: %v", queryID, err)
		return planfresh.Plan{Source: planfresh.SourceNone}
	}
	if p.StaleReason != "" {
		t.logFn("DEBUG", "tuner: cached plan of queryid=%d not used: %s", queryID,
			p.StaleReason)
	}
	return p
}

// explainOrNone is the tuner's fallback EXPLAIN: a statement it cannot plan
// afresh has no plan, and the reader still reports the stale plan's
// relations to the deferral gate.
func (t *Tuner) explainOrNone(queryID int64) planfresh.ExplainFunc {
	if t.planExplain == nil {
		return nil
	}
	return func(ctx context.Context, text string) (planfresh.Plan, error) {
		p, err := t.planExplain(ctx, text)
		if err != nil {
			t.logFn("WARN", "tuner: cannot plan queryid=%d afresh: %v", queryID, err)
			return planfresh.Plan{Source: planfresh.SourceNone}, nil
		}
		return p, nil
	}
}

// deferralPlanJSON is the plan whose relations the deferral gate reads:
// the fresh plan, else the newest stale one. A statement reads the same
// tables after an ANALYZE, so a stale plan still defers it (fail closed).
func (t *Tuner) deferralPlanJSON(ctx context.Context, queryID int64) string {
	if j := t.fetchPlanJSON(ctx, queryID); j != "" {
		return j
	}
	return string(t.cyclePlan(ctx, queryID).StaleJSON)
}

// cyclePlan is queryID's plan: the one read for it this pass, else a read.
func (t *Tuner) cyclePlan(ctx context.Context, queryID int64) planfresh.Plan {
	if p, ok := t.plans[queryID]; ok {
		return p
	}
	return t.readPlan(ctx, queryID, "")
}

func (t *Tuner) fetchPlanJSON(ctx context.Context, queryID int64) string {
	return string(t.cyclePlan(ctx, queryID).JSON)
}

// planProvenance is the finding detail's plan source, capture time and age.
func (t *Tuner) planProvenance(ctx context.Context, queryID int64) map[string]any {
	return t.cyclePlan(ctx, queryID).Provenance(time.Now())
}

func (t *Tuner) scanPlanForQuery(ctx context.Context, queryID int64) []PlanSymptom {
	p := t.cyclePlan(ctx, queryID)
	if len(p.JSON) == 0 {
		return nil
	}
	var opts []ScanOption
	if !p.HasActuals {
		opts = append(opts, WithEstimatesOnly())
	}
	symptoms, err := t.scanPlan(ctx, p.JSON, opts...)
	if err != nil {
		t.logFn("tuner", "scan plan for queryid %d: %v", queryID, err)
		return nil
	}
	return symptoms
}
