package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/review/modelgate"
)

// The model gate's wiring (review-05a): after each grade, the database
// runtime evaluates G-M1 and G-M2 for the review model it would run now, in
// the database's own sage schema. A pass files one promotion in the inbox;
// only an admin's approval (POST .../review/gate/{id}/approve) grants. A
// failed window or a change of the review model revokes at once.

// reviewGateTick is the evaluation's tick: one evaluation per hour per
// model and rule, however many grades or sidecars run in it.
const reviewGateTick = time.Hour

// reviewModelName is the model the review pass would run on, as
// llm.Manager.ForPurpose("review") picks its client: llm.review_llm when
// enabled, else llm.optimizer_llm when enabled, else the general model;
// "" when the model layer is off.
func reviewModelName(c *config.Config) string {
	if c == nil || !c.LLM.Enabled {
		return ""
	}
	switch {
	case c.LLM.ReviewLLM.Enabled:
		return reviewLLMConfig(c.LLM).Model
	case c.LLM.OptimizerLLM.Enabled:
		return optimizerLLMConfig(c.LLM).Model
	}
	return c.LLM.Model
}

// reviewGate is the runtime's gate over its own database.
func (rt *databaseRuntime) reviewGate() *modelgate.Gate {
	c := rt.cfg
	if c == nil {
		c = config.DefaultConfig()
	}
	g := c.Review.Gate
	return modelgate.New(rt.spec.Pool, rt.spec.Name, modelgate.Options{
		Thresholds: modelgate.ThresholdsFrom(g), Fixtures: g.FixtureDatabases,
		OperatorOn: func() bool { return c.Review.ModelPass == config.ReviewModelPassOn },
	})
}

// evaluateReviewGate runs one evaluation and logs what changed. An error
// leaves the gate as it was (never eligible by default).
func (rt *databaseRuntime) evaluateReviewGate(ctx context.Context) {
	if rt.spec.Pool == nil {
		return
	}
	model := reviewModelName(rt.cfg)
	res, err := rt.reviewGate().Evaluate(ctx, model, time.Now().Truncate(reviewGateTick))
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: review model gate not evaluated: %v", rt.spec.Name,
			err)
		return
	}
	for _, m := range res.Revoked {
		logWarn(rt.spec.Scope, "db %q: review model gate revoked %s: the review model is "+
			"now %q", rt.spec.Name, m, res.Model)
	}
	if res.Model == "" {
		return
	}
	logInfo(rt.spec.Scope, "db %q: review model gate for %s: G-M1 %s (%s); G-M2 %s",
		rt.spec.Name, res.Model, res.M1State, res.M1.Reason, res.M2State)
}
