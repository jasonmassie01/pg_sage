package main

import (
	"fmt"
	"sync"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/changereview/reasoner"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Change Review wiring. Each database runtime builds its own service: the
// impact engine and the reasoner read only that database's pool, reviews
// are stored in its own sage.change_review, and the runner's goroutines
// run on the runtime's worker group. The service is registered in one
// process-wide registry that the REST routes and the MCP tools route
// through, and removed when the runtime stops. The reasoner always runs:
// without a model it keeps reviews deterministic (model_status no_model)
// and gives ORM-only migrations CR-20.

var (
	changeReviewRegistryOnce sync.Once
	changeReviewRegistry     *changereview.Registry
)

// changeReviewServices is the process-wide Change Review registry.
func changeReviewServices() *changereview.Registry {
	changeReviewRegistryOnce.Do(func() { changeReviewRegistry = changereview.NewRegistry() })
	return changeReviewRegistry
}

// startChangeReview registers the database's Change Review until the
// runtime stops.
func (rt *databaseRuntime) startChangeReview() {
	svc, err := rt.newChangeReview()
	if err == nil {
		_, err = svc.Start(rt.ctx)
	}
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: Change Review not started: %v", rt.spec.Name, err)
		return
	}
	changeReviewServices().Set(rt.spec.Name, svc)
	rt.start(func() {
		<-rt.ctx.Done()
		changeReviewServices().Remove(rt.spec.Name, svc)
	})
	rt.note("change_review")
}

// newChangeReview builds the service on the runtime's pool, model and
// dispatcher.
func (rt *databaseRuntime) newChangeReview() (*changereview.Service, error) {
	crCfg := rt.cfg.ChangeReview
	// The stages log only what failed or degraded.
	logf := func(format string, args ...any) { logWarn(rt.spec.Scope, format, args...) }
	engine, err := impact.New(impact.Deps{Pool: rt.spec.Pool, Config: crCfg,
		Timeouts: catalogread.FromSafety(rt.cfg.Safety), Logf: logf})
	if err != nil {
		return nil, fmt.Errorf("impact engine: %w", err)
	}
	budget := changereview.NewBudget(rt.spec.Pool, int64(crCfg.DailyTokensPerDatabase),
		int64(crCfg.DailyTokensPerActor), nil)
	r, err := reasoner.New(reasoner.Deps{Model: changeReviewModel(rt.llmManager),
		Tools: engine, Budget: budget, Logf: logf})
	if err != nil {
		return nil, fmt.Errorf("reasoner: %w", err)
	}
	render := changereview.RenderOptions{}
	return changereview.New(changereview.Deps{Database: rt.spec.Name, Pool: rt.spec.Pool,
		Config: crCfg, Analyzer: engine, Reasoner: r,
		Notifier:   newChangeReviewNotifier(rt.dispatcher, render, logf),
		HunkFilter: diffscan.HunkFilter, Redact: diffscan.Redact, Render: render,
		Go: rt.start, Logf: logf})
}

// changeReviewModel is the client for the change_review purpose (the
// general one), or no model. The reasoner checks per review whether the
// client is switched on, so a hot-reloaded llm.enabled takes effect.
func changeReviewModel(m *llm.Manager) agentloop.Model {
	if m == nil {
		return nil
	}
	if c := m.ForPurpose(reasoner.Purpose); c != nil {
		return c
	}
	return nil
}
